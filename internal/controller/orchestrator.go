package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// Broadcaster sends commands to all connected clients.
type Broadcaster interface {
	Broadcast(env protocol.Envelope)
	SendTo(clientID string, env protocol.Envelope)
}

// Orchestrator drives the global test phase sequence.
type Orchestrator struct {
	mu sync.RWMutex

	cfg        protocol.Config
	registry   *Registry
	metrics    *MetricsStore
	broadcast  Broadcaster
	running    bool
	phase      protocol.Phase
	percent    int
	startedAt  *time.Time
	statusText string
	cancel     context.CancelFunc
	phaseSpans []protocol.PhaseSpan

	// filesCreatedApprox is updated from client status reports.
	filesCreated int64
}

func NewOrchestrator(reg *Registry, metrics *MetricsStore, bc Broadcaster) *Orchestrator {
	return &Orchestrator{
		cfg:        protocol.DefaultConfig(),
		registry:   reg,
		metrics:    metrics,
		broadcast:  bc,
		phase:      protocol.PhaseIdle,
		statusText: "Ready",
	}
}

func (o *Orchestrator) Config() protocol.Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.cfg
}

func (o *Orchestrator) SetConfig(cfg protocol.Config) error {
	if cfg.TestName == "" {
		return fmt.Errorf("test_name is required")
	}
	if len(cfg.Prefixes) == 0 {
		return fmt.Errorf("at least one prefix is required")
	}
	if cfg.FileCreationRate <= 0 || cfg.FileDeletionRate <= 0 {
		return fmt.Errorf("creation and deletion rates must be > 0")
	}
	if cfg.FileWriteBandwidth <= 0 || cfg.FileReadBandwidth <= 0 {
		return fmt.Errorf("bandwidth values must be > 0")
	}
	if cfg.PhaseStepSeconds <= 0 {
		cfg.PhaseStepSeconds = protocol.DefaultPhaseStepDuration.Seconds()
	}
	if cfg.PhaseStepSeconds < 1 {
		return fmt.Errorf("phase_step_seconds must be >= 1")
	}
	if strings.TrimSpace(cfg.PackageURL) == "" {
		cfg.PackageURL = protocol.DefaultPackageURL
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return fmt.Errorf("cannot change config while running")
	}
	o.cfg = cfg
	o.registry.ReassignPrefixes(cfg.Prefixes)
	o.broadcast.Broadcast(protocol.Envelope{Type: "config", Config: &cfg})
	return nil
}

func (o *Orchestrator) Snapshot() protocol.UIState {
	o.mu.RLock()
	defer o.mu.RUnlock()
	var elapsed float64
	if o.startedAt != nil {
		elapsed = time.Since(*o.startedAt).Seconds()
	}
	spans := make([]protocol.PhaseSpan, len(o.phaseSpans))
	copy(spans, o.phaseSpans)
	return protocol.UIState{
		Config:      o.cfg,
		Clients:     o.registry.List(),
		Running:     o.running,
		Phase:       o.phase,
		Percent:     o.percent,
		StartedAt:   o.startedAt,
		ElapsedSec:  elapsed,
		History:        o.metrics.History(),
		Latencies:      o.metrics.Latencies(),
		LatencyEdgesUs: append([]float64(nil), protocol.LatencyBucketEdgesUs...),
		PhaseSpans:     spans,
		PhaseOrder:     protocol.EffectivePhaseOrder(o.cfg),
		StatusText:  o.statusText,
		ClientCount: o.registry.Count(),
	}
}

func (o *Orchestrator) UpdateClientFiles(n int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n > o.filesCreated {
		o.filesCreated = n
	}
}

func (o *Orchestrator) Start() error {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return fmt.Errorf("already running")
	}
	n := o.registry.Count()
	if n == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no clients registered")
	}
	cfg := o.cfg
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.running = true
	now := time.Now()
	o.startedAt = &now
	o.phase = protocol.PhaseCreate
	o.percent = 0
	o.filesCreated = 0
	o.phaseSpans = nil
	o.statusText = "Starting create phase"
	o.mu.Unlock()

	o.metrics.Begin()
	go o.run(ctx, cfg, n)
	return nil
}

func (o *Orchestrator) Stop() {
	o.mu.Lock()
	cancel := o.cancel
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	cleanup := true
	o.broadcast.Broadcast(protocol.Envelope{
		Type: "stop",
		Stop: &protocol.StopMsg{Cleanup: cleanup},
	})
	o.metrics.Freeze()
	o.mu.Lock()
	o.closePhaseSpanLocked(time.Now())
	o.running = false
	o.phase = protocol.PhaseStopped
	o.percent = 0
	o.statusText = "Stopped — clients cleaning up"
	o.cancel = nil
	o.mu.Unlock()
}

func (o *Orchestrator) IsRunning() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.running
}

func (o *Orchestrator) setPhase(phase protocol.Phase, percent int, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if phase != o.phase || len(o.phaseSpans) == 0 {
		now := time.Now()
		o.closePhaseSpanLocked(now)
		o.phaseSpans = append(o.phaseSpans, protocol.PhaseSpan{
			Phase: phase,
			Start: now,
		})
	}
	o.phase = phase
	o.percent = percent
	o.statusText = text
}

func (o *Orchestrator) closePhaseSpanLocked(at time.Time) {
	if n := len(o.phaseSpans); n > 0 && o.phaseSpans[n-1].End == nil {
		end := at
		o.phaseSpans[n-1].End = &end
	}
}

func (o *Orchestrator) finish(text string) {
	o.metrics.Freeze()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closePhaseSpanLocked(time.Now())
	o.running = false
	o.phase = protocol.PhaseIdle
	o.percent = 0
	o.statusText = text
	o.cancel = nil
}

func (o *Orchestrator) perClient(global float64, n int) float64 {
	if n < 1 {
		n = 1
	}
	return global / float64(n)
}

func (o *Orchestrator) run(ctx context.Context, cfg protocol.Config, nClients int) {
	defer func() {
		if ctx.Err() != nil {
			o.finish("Stopped")
			return
		}
		o.finish("Completed")
	}()

	createRate := o.perClient(cfg.FileCreationRate, nClients)
	deleteRate := o.perClient(cfg.FileDeletionRate, nClients)
	writeBW := o.perClient(cfg.FileWriteBandwidth, nClients)
	readBW := o.perClient(cfg.FileReadBandwidth, nClients)
	step := cfg.PhaseStepDuration()

	log.Printf("orchestrator: start with %d clients step=%s create=%.1f/s delete=%.1f/s write=%.0f B/s read=%.0f B/s software=%v",
		nClients, step, createRate, deleteRate, writeBW, readBW, cfg.SoftwareEnabled())

	// Optional: unpack package into <prefix>/software, then cold + warm startup.
	if cfg.SoftwareEnabled() {
		if err := o.runSoftwarePhase(ctx, protocol.PhaseSoftwareUnpack, protocol.SoftwareUnpackTimeout); err != nil {
			return
		}
		if err := o.runSoftwarePhase(ctx, protocol.PhaseSoftwareCold, protocol.SoftwareStartupPhaseTimeout); err != nil {
			return
		}
		if err := o.runSoftwarePhase(ctx, protocol.PhaseSoftwareWarm, protocol.SoftwareStartupPhaseTimeout); err != nil {
			return
		}
	}

	// 1) Create ramp
	if err := o.runRamp(ctx, protocol.PhaseCreate, createRate, protocol.CreateFileSize, step); err != nil {
		return
	}

	// 2) Delete ramp (full ladder + extra 100% sweep for leftovers)
	if err := o.runRamp(ctx, protocol.PhaseDelete, deleteRate, 0, step); err != nil {
		return
	}
	if err := o.sendAndWait(ctx, protocol.PhaseDelete, 100, deleteRate, 0, 0, step); err != nil {
		return
	}

	// 3) Write bandwidth
	if err := o.runRamp(ctx, protocol.PhaseWriteBW, writeBW, protocol.BandwidthFileSize, step); err != nil {
		return
	}

	// 4) Read bandwidth
	if err := o.runRamp(ctx, protocol.PhaseReadBW, readBW, protocol.BandwidthFileSize, step); err != nil {
		return
	}

	// 5) Overlapped read+write at the same downscaled ramp targets
	if err := o.runRamp2(ctx, protocol.PhaseReadWrite, writeBW, readBW, protocol.BandwidthFileSize, step); err != nil {
		return
	}

	// 6) Final delete
	if err := o.runRamp(ctx, protocol.PhaseFinalDelete, deleteRate, 0, step); err != nil {
		return
	}
	_ = o.sendAndWait(ctx, protocol.PhaseFinalDelete, 100, deleteRate, 0, 0, step)
}

func (o *Orchestrator) runSoftwarePhase(ctx context.Context, phase protocol.Phase, timeout time.Duration) error {
	cfg := o.Config()
	o.setPhase(phase, 100, protocol.PhaseLabel(phase))

	cmd := &protocol.PhaseCommand{
		Phase:          phase,
		Percent:        100,
		Duration:       timeout.Seconds(),
		TestName:       cfg.TestName,
		PackageURL:     cfg.PackageURL,
		StartupCommand: cfg.StartupCommand,
	}
	o.broadcast.Broadcast(protocol.Envelope{Type: "command", Command: cmd})
	log.Printf("orchestrator: phase=%s timeout=%s package=%s", phase, timeout, cfg.PackageURL)

	return o.waitClientsIdle(ctx, timeout)
}

// waitClientsIdle waits until every registered client reports idle after starting work,
// or until timeout. Used for one-shot software phases that finish early.
func (o *Orchestrator) waitClientsIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	sawRunning := false
	grace := time.Now().Add(1500 * time.Millisecond)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				log.Printf("orchestrator: software phase wait timed out after %s", timeout)
				return nil
			}
			clients := o.registry.List()
			if len(clients) == 0 {
				continue
			}
			allIdle := true
			anyRunning := false
			for _, c := range clients {
				switch c.Status {
				case "running":
					anyRunning = true
					sawRunning = true
					allIdle = false
				case "idle":
					// finished this step
				default:
					allIdle = false
				}
			}
			if sawRunning && allIdle && !anyRunning {
				return nil
			}
			// Fast path: everyone already idle and we waited past grace (finished before first poll).
			if allIdle && !anyRunning && time.Now().After(grace) && (sawRunning || time.Since(grace) > 2*time.Second) {
				return nil
			}
		}
	}
}

func (o *Orchestrator) runRamp(ctx context.Context, phase protocol.Phase, baseRate float64, fileSize int64, step time.Duration) error {
	for _, pct := range protocol.RampPercents {
		rate := baseRate * float64(pct) / 100.0
		if err := o.sendAndWait(ctx, phase, pct, rate, 0, fileSize, step); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) runRamp2(ctx context.Context, phase protocol.Phase, writeBase, readBase float64, fileSize int64, step time.Duration) error {
	for _, pct := range protocol.RampPercents {
		wr := writeBase * float64(pct) / 100.0
		rr := readBase * float64(pct) / 100.0
		if err := o.sendAndWait(ctx, phase, pct, wr, rr, fileSize, step); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) sendAndWait(ctx context.Context, phase protocol.Phase, pct int, rate, readRate float64, fileSize int64, step time.Duration) error {
	cfg := o.Config()
	o.setPhase(phase, pct, fmt.Sprintf("%s @ %d%%", protocol.PhaseLabel(phase), pct))

	cmd := &protocol.PhaseCommand{
		Phase:    phase,
		Percent:  pct,
		Duration: step.Seconds(),
		Rate:     rate,
		ReadRate: readRate,
		TestName: cfg.TestName,
		FileSize: fileSize,
	}
	o.broadcast.Broadcast(protocol.Envelope{Type: "command", Command: cmd})
	log.Printf("orchestrator: phase=%s percent=%d rate=%.2f read_rate=%.2f duration=%s", phase, pct, rate, readRate, step)

	timer := time.NewTimer(step)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
