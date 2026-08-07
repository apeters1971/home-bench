package client

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
	"github.com/gorilla/websocket"
)

// Agent connects to the controller, runs phase commands, and reports metrics.
type Agent struct {
	ControllerURL string
	Hostname      string

	mu       sync.Mutex
	conn     *websocket.Conn
	clientID string
	prefix   string
	config   protocol.Config
	ledger   *FileLedger
	stats    *Stats
	worker   *Worker

	runCancel context.CancelFunc
	cmdCh     chan protocol.PhaseCommand
}

func NewAgent(controllerURL, hostname string) *Agent {
	if hostname == "" {
		h, _ := os.Hostname()
		hostname = h
	}
	stats := &Stats{}
	ledger := NewFileLedger()
	return &Agent{
		ControllerURL: controllerURL,
		Hostname:      hostname,
		ledger:        ledger,
		stats:         stats,
		cmdCh:         make(chan protocol.PhaseCommand, 8),
	}
}

func (a *Agent) Run(ctx context.Context) error {
	u, err := url.Parse(a.ControllerURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = "/ws/client"

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := a.session(ctx, u.String()); err != nil {
			log.Printf("session ended: %v — reconnecting in 2s", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
	}
}

func (a *Agent) session(ctx context.Context, wsURL string) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	defer func() {
		a.stopWork()
		_ = conn.Close()
	}()

	reg := protocol.Envelope{
		Type: "register",
		Register: &protocol.RegisterMsg{
			Hostname: a.Hostname,
			ID:       a.clientID,
		},
	}
	if err := conn.WriteJSON(reg); err != nil {
		return err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go a.metricsLoop(sessCtx)
	go a.commandLoop(sessCtx)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			cancel()
			return err
		}
		switch env.Type {
		case "welcome":
			if env.Welcome == nil {
				continue
			}
			a.mu.Lock()
			a.clientID = env.Welcome.ClientID
			a.prefix = env.Welcome.Prefix
			a.config = env.Welcome.Config
			a.worker = NewWorker(a.Hostname, a.prefix, a.ledger, a.stats)
			a.mu.Unlock()
			log.Printf("registered id=%s prefix=%s", env.Welcome.ClientID, env.Welcome.Prefix)
			a.sendStatus("registered", protocol.PhaseIdle, 0, "ready", a.ledger.Count())

		case "config":
			if env.Config != nil {
				a.mu.Lock()
				a.config = *env.Config
				a.prefix = protocol.SelectPrefix(a.Hostname, env.Config.Prefixes)
				a.worker = NewWorker(a.Hostname, a.prefix, a.ledger, a.stats)
				a.mu.Unlock()
				log.Printf("config updated prefix=%s test=%s", a.prefix, env.Config.TestName)
			}

		case "command":
			if env.Command == nil {
				continue
			}
			cmd := *env.Command
			a.mu.Lock()
			if a.prefix != "" {
				cmd.Prefix = a.prefix
			}
			a.mu.Unlock()
			// Replace any queued command with the latest.
			select {
			case a.cmdCh <- cmd:
			default:
				select {
				case <-a.cmdCh:
				default:
				}
				a.cmdCh <- cmd
			}

		case "stop":
			a.stopWork()
			cleanup := env.Stop == nil || env.Stop.Cleanup
			if cleanup {
				a.mu.Lock()
				cfg := a.config
				prefix := a.prefix
				w := a.worker
				a.mu.Unlock()
				if w != nil {
					log.Printf("cleanup under %s", HostRoot(prefix, cfg.TestName, a.Hostname))
					_ = w.Cleanup(prefix, cfg.TestName)
				}
			}
			a.sendStatus("stopped", protocol.PhaseStopped, 0, "cleaned up", 0)
		}
	}
}

func (a *Agent) commandLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-a.cmdCh:
			a.stopWork()
			workCtx, cancel := context.WithCancel(ctx)
			a.mu.Lock()
			a.runCancel = cancel
			w := a.worker
			a.mu.Unlock()
			if w == nil {
				cancel()
				continue
			}
			a.sendStatus("running", cmd.Phase, cmd.Percent, "executing", a.ledger.Count())
			log.Printf("phase=%s percent=%d rate=%.2f dur=%.0fs", cmd.Phase, cmd.Percent, cmd.Rate, cmd.Duration)
			if err := w.Run(workCtx, cmd); err != nil && err != context.Canceled {
				log.Printf("phase error: %v", err)
			}
			a.sendStatus("idle", cmd.Phase, cmd.Percent, "step done", a.ledger.Count())
		}
	}
}

func (a *Agent) stopWork() {
	a.mu.Lock()
	cancel := a.runCancel
	a.runCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *Agent) metricsLoop(ctx context.Context) {
	t := time.NewTicker(protocol.MetricsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sample := a.stats.SnapshotAndReset()
			a.mu.Lock()
			sample.ClientID = a.clientID
			conn := a.conn
			a.mu.Unlock()
			if sample.ClientID == "" || conn == nil {
				continue
			}
			// Skip empty idle samples to reduce noise, but keep a heartbeat via status.
			if sample.ReadOps == 0 && sample.WriteOps == 0 && sample.CreateOps == 0 &&
				sample.DeleteOps == 0 && sample.ReadBytes == 0 && sample.WriteBytes == 0 {
				continue
			}
			env := protocol.Envelope{Type: "metrics", Metrics: &sample}
			a.mu.Lock()
			err := conn.WriteJSON(env)
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (a *Agent) sendStatus(status string, phase protocol.Phase, percent int, detail string, files int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil || a.clientID == "" {
		return
	}
	env := protocol.Envelope{
		Type: "status",
		Status: &protocol.StatusMsg{
			ClientID: a.clientID,
			Status:   status,
			Phase:    phase,
			Percent:  percent,
			Detail:   detail,
			Files:    files,
		},
	}
	_ = a.conn.WriteJSON(env)
}
