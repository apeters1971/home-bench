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
	stats := NewStats()
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
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		a.mu.Unlock()
		_ = conn.Close()
	}()

	_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
		return nil
	})

	reg := protocol.Envelope{
		Type: "register",
		Register: &protocol.RegisterMsg{
			Hostname: a.Hostname,
			ID:       a.clientID,
		},
	}
	if err := a.writeJSON(conn, reg); err != nil {
		return err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go a.metricsLoop(sessCtx, conn)
	go a.pingLoop(sessCtx, conn)
	go a.commandLoop(sessCtx)

	for {
		var env protocol.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			cancel()
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(protocol.WSReadTimeout))
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

func (a *Agent) pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(protocol.WSPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
			a.mu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (a *Agent) metricsLoop(ctx context.Context, conn *websocket.Conn) {
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
			a.mu.Unlock()
			if sample.ClientID == "" {
				continue
			}
			// Always send — zeros keep the controller from pruning during quiet
			// delete/wait gaps and keep the UI history advancing.
			env := protocol.Envelope{Type: "metrics", Metrics: &sample}
			if err := a.writeJSON(conn, env); err != nil {
				return
			}
		}
	}
}

func (a *Agent) writeJSON(conn *websocket.Conn, env protocol.Envelope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteJSON(env)
}

func (a *Agent) sendStatus(status string, phase protocol.Phase, percent int, detail string, files int64) {
	a.mu.Lock()
	conn := a.conn
	id := a.clientID
	a.mu.Unlock()
	if conn == nil || id == "" {
		return
	}
	env := protocol.Envelope{
		Type: "status",
		Status: &protocol.StatusMsg{
			ClientID: id,
			Status:   status,
			Phase:    phase,
			Percent:  percent,
			Detail:   detail,
			Files:    files,
		},
	}
	_ = a.writeJSON(conn, env)
}
