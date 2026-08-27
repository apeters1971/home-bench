package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
	"github.com/apeters/homebench/internal/software"
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
	configPath string
	hostname   string

	// selectedIDs is the UI selection for the next run.
	selectedIDs map[string]struct{}
	// autoSelectNew adds registrants to selectedIDs while idle (off after Start or a partial selection).
	autoSelectNew bool
	// runClientIDs is frozen at Start; commands/stop go only to these clients.
	runClientIDs []string

	running    bool
	phase      protocol.Phase
	percent    int
	startedAt  *time.Time
	elapsedSec float64 // frozen when the run ends (Completed / Stopped)
	statusText string
	failMessage string // non-empty → run ends as FAILED RUN
	cancel     context.CancelFunc
	phaseSpans []protocol.PhaseSpan

	// filesCreatedApprox is updated from client status reports.
	filesCreated int64
}

func NewOrchestrator(reg *Registry, metrics *MetricsStore, bc Broadcaster, configPath string) *Orchestrator {
	cfg := protocol.DefaultConfig()
	if configPath != "" {
		loaded, err := LoadConfigFile(configPath)
		if err == nil {
			cfg = loaded
			log.Printf("loaded config from %s", configPath)
		} else if os.IsNotExist(err) {
			if err := SaveConfigFile(configPath, cfg); err != nil {
				log.Printf("could not write default config to %s: %v", configPath, err)
			} else {
				log.Printf("wrote default config to %s", configPath)
			}
		} else {
			log.Printf("config load %s: %v — using defaults", configPath, err)
		}
	}
	hostname, _ := os.Hostname()
	return &Orchestrator{
		cfg:         cfg,
		registry:    reg,
		metrics:     metrics,
		broadcast:   bc,
		configPath:  configPath,
		hostname:    hostname,
		selectedIDs:   make(map[string]struct{}),
		autoSelectNew: true,
		phase:         protocol.PhaseIdle,
		statusText:    "Ready",
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
	if o.configPath != "" {
		if err := SaveConfigFile(o.configPath, cfg); err != nil {
			log.Printf("config save %s: %v", o.configPath, err)
		}
	}
	return nil
}

func (o *Orchestrator) Snapshot() protocol.UIState {
	o.mu.RLock()
	defer o.mu.RUnlock()
	elapsed := o.elapsedSec
	if o.running && o.startedAt != nil {
		elapsed = time.Since(*o.startedAt).Seconds()
	}
	spans := make([]protocol.PhaseSpan, len(o.phaseSpans))
	copy(spans, o.phaseSpans)

	clients := o.registry.List()
	selectedIDs := make([]string, 0, len(o.selectedIDs))
	participantCount := 0
	if o.running && len(o.runClientIDs) > 0 {
		runSet := make(map[string]struct{}, len(o.runClientIDs))
		for _, id := range o.runClientIDs {
			runSet[id] = struct{}{}
		}
		for i := range clients {
			_, sel := runSet[clients[i].ID]
			clients[i].Selected = sel
			if sel {
				participantCount++
				selectedIDs = append(selectedIDs, clients[i].ID)
			}
		}
	} else {
		for i := range clients {
			_, sel := o.selectedIDs[clients[i].ID]
			clients[i].Selected = sel
			if sel {
				participantCount++
				selectedIDs = append(selectedIDs, clients[i].ID)
			}
		}
	}

	return protocol.UIState{
		Config:             o.cfg,
		Clients:            clients,
		Running:            o.running,
		Phase:              o.phase,
		Percent:            o.percent,
		StartedAt:          o.startedAt,
		ElapsedSec:         elapsed,
		History:            o.metrics.History(),
		Latencies:          o.metrics.Latencies(),
		LatencyEdgesUs:     append([]float64(nil), protocol.LatencyBucketEdgesUs...),
		PhaseSpans:         spans,
		PhaseOrder:         protocol.EffectivePhaseOrder(o.cfg),
		StatusText:         o.statusText,
		ClientCount:        len(clients),
		ParticipantCount:   participantCount,
		SelectedClientIDs:  selectedIDs,
		ControllerHostname: o.hostname,
	}
}

// OnClientRegistered may select a newly connected client for the next run.
// Never while a run is active, and not after Start / a partial lock has disabled auto-select.
func (o *Orchestrator) OnClientRegistered(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running || !o.autoSelectNew {
		return
	}
	if o.selectedIDs == nil {
		o.selectedIDs = make(map[string]struct{})
	}
	o.selectedIDs[id] = struct{}{}
}

// OnClientRemoved drops a client from the selection set.
// If that client is a frozen run participant, the run is aborted as FAILED.
func (o *Orchestrator) OnClientRemoved(id, hostname string) {
	o.mu.Lock()
	delete(o.selectedIDs, id)
	if !o.running || o.failMessage != "" {
		o.mu.Unlock()
		return
	}
	participant := false
	for _, x := range o.runClientIDs {
		if x == id {
			participant = true
			break
		}
	}
	if !participant {
		o.mu.Unlock()
		return
	}
	label := strings.TrimSpace(hostname)
	if label == "" {
		label = id
		if len(label) > 8 {
			label = label[:8]
		}
	}
	msg := fmt.Sprintf("FAILED RUN — participant lost (%s)", label)
	cancel := o.cancel
	o.cancel = nil
	o.failMessage = msg
	o.statusText = msg
	o.phase = protocol.PhaseStopped
	o.percent = 0
	o.closePhaseSpanLocked(time.Now())
	if o.startedAt != nil {
		o.elapsedSec = time.Since(*o.startedAt).Seconds()
	}
	o.mu.Unlock()

	log.Printf("orchestrator: %s", msg)
	if cancel != nil {
		cancel()
	}
	o.broadcastRun(protocol.Envelope{
		Type: "stop",
		Stop: &protocol.StopMsg{Cleanup: true},
	})
	o.metrics.Freeze()
}

// SetSelectedClients replaces the participant selection (idle only).
func (o *Orchestrator) SetSelectedClients(ids []string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return fmt.Errorf("cannot change participants while running")
	}
	known := make(map[string]struct{})
	for _, id := range o.registry.IDs() {
		known[id] = struct{}{}
	}
	next := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := known[id]; !ok {
			continue
		}
		next[id] = struct{}{}
	}
	o.selectedIDs = next
	// Only keep auto-including newcomers when every currently known client is selected.
	o.autoSelectNew = len(next) > 0 && len(next) == len(known)
	return nil
}

func (o *Orchestrator) IsParticipant(id string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if !o.running {
		return false
	}
	for _, x := range o.runClientIDs {
		if x == id {
			return true
		}
	}
	return false
}

func (o *Orchestrator) broadcastRun(env protocol.Envelope) {
	o.mu.RLock()
	ids := append([]string(nil), o.runClientIDs...)
	o.mu.RUnlock()
	for _, id := range ids {
		o.broadcast.SendTo(id, env)
	}
}

func (o *Orchestrator) connectedSelectedLocked() []string {
	known := make(map[string]struct{})
	for _, id := range o.registry.IDs() {
		known[id] = struct{}{}
	}
	out := make([]string, 0, len(o.selectedIDs))
	for id := range o.selectedIDs {
		if _, ok := known[id]; ok {
			out = append(out, id)
		}
	}
	return out
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
	if o.registry.Count() == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no clients registered")
	}
	participants := o.connectedSelectedLocked()
	if len(participants) == 0 {
		o.mu.Unlock()
		return fmt.Errorf("no clients selected")
	}
	n := len(participants)
	cfg := o.cfg
	ctx, cancel := context.WithCancel(context.Background())
	o.cancel = cancel
	o.running = true
	o.runClientIDs = participants
	o.autoSelectNew = false // lock set: newcomers stay unselected until explicitly chosen
	o.failMessage = ""
	now := time.Now()
	o.startedAt = &now
	o.elapsedSec = 0
	o.phase = protocol.PhaseCreate
	o.percent = 0
	o.filesCreated = 0
	o.phaseSpans = nil
	o.statusText = fmt.Sprintf("Starting create phase (%d clients)", n)
	o.mu.Unlock()

	o.metrics.Begin()
	log.Printf("orchestrator: starting with %d selected of %d registered", n, o.registry.Count())
	go o.run(ctx, cfg, n)
	return nil
}

func (o *Orchestrator) Stop() {
	o.mu.Lock()
	if !o.running {
		o.mu.Unlock()
		return
	}
	if o.failMessage != "" {
		// Already failing; leave fail status in place.
		o.mu.Unlock()
		return
	}
	cancel := o.cancel
	o.cancel = nil
	o.phase = protocol.PhaseStopped
	o.percent = 0
	o.statusText = "Stopped — clients cleaning up"
	o.closePhaseSpanLocked(time.Now())
	if o.startedAt != nil {
		o.elapsedSec = time.Since(*o.startedAt).Seconds()
	}
	// Keep running=true and runClientIDs until finish() so participant
	// selection stays locked through client cleanup.
	o.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	o.broadcastRun(protocol.Envelope{
		Type: "stop",
		Stop: &protocol.StopMsg{Cleanup: true},
	})
	o.metrics.Freeze()
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
	if o.startedAt != nil {
		o.elapsedSec = time.Since(*o.startedAt).Seconds()
	}
	if o.failMessage != "" {
		text = o.failMessage
	}
	o.running = false
	o.runClientIDs = nil
	o.failMessage = ""
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
		o.mu.RLock()
		failed := o.failMessage != ""
		o.mu.RUnlock()
		if failed {
			o.finish("FAILED RUN")
			return
		}
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

	log.Printf("orchestrator: start with %d clients step=%s create=%.1f/s delete=%.1f/s write=%.0f B/s read=%.0f B/s software=%v git=%v untar=%v",
		nClients, step, createRate, deleteRate, writeBW, readBW, cfg.SoftwareEnabled(), cfg.GitCloneEnabled(), cfg.UntarEnabled())

	// Optional: controller unpacks package once into each <prefix>/<test>/software,
	// then clients run cold + warm startup from that shared tree.
	if cfg.SoftwareEnabled() {
		if err := o.runControllerUnpack(ctx); err != nil {
			log.Printf("orchestrator: software unpack failed: %v", err)
			return
		}
		if err := o.runSoftwarePhase(ctx, protocol.PhaseSoftwareCold, protocol.SoftwareStartupColdTimeout); err != nil {
			return
		}
		if err := o.runSoftwarePhase(ctx, protocol.PhaseSoftwareWarm, protocol.SoftwareStartupWarmTimeout); err != nil {
			return
		}
		// One controller-side cleanup (not N concurrent client deletes on the shared tree).
		o.cleanupSoftwareAsync(cfg)
	}

	// Optional: git clone and/or tar xvf into <prefix>/<test>/software.
	if cfg.GitCloneEnabled() {
		if err := o.runSoftwarePhase(ctx, protocol.PhaseGitClone, protocol.SoftwareOpTimeout); err != nil {
			return
		}
	}
	if cfg.UntarEnabled() {
		if err := o.runSoftwarePhase(ctx, protocol.PhaseUntar, protocol.SoftwareOpTimeout); err != nil {
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

	// 6) Final delete (paced ramp), then forced wipe of anything left past the window.
	if err := o.runRamp(ctx, protocol.PhaseFinalDelete, deleteRate, 0, step); err != nil {
		return
	}
	if err := o.sendFinalCleanup(ctx); err != nil {
		return
	}
}

func (o *Orchestrator) runControllerUnpack(ctx context.Context) error {
	cfg := o.Config()
	o.setPhase(protocol.PhaseSoftwareUnpack, 100, protocol.PhaseLabel(protocol.PhaseSoftwareUnpack))
	log.Printf("orchestrator: controller unpacking %s into %d prefix(es)", cfg.PackageURL, len(cfg.Prefixes))

	ctx, cancel := context.WithTimeout(ctx, protocol.SoftwareUnpackTimeout)
	defer cancel()

	if err := software.UnpackToPrefixes(ctx, cfg.PackageURL, cfg.Prefixes, cfg.TestName); err != nil {
		return err
	}
	log.Printf("orchestrator: software unpack complete")
	return nil
}

// cleanupSoftwareAsync removes shared <prefix>/<test>/software trees without blocking the run.
func (o *Orchestrator) cleanupSoftwareAsync(cfg protocol.Config) {
	prefixes := append([]string(nil), cfg.Prefixes...)
	testName := cfg.TestName
	go func() {
		for _, prefix := range prefixes {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" {
				continue
			}
			dir := software.Dir(prefix, testName)
			log.Printf("orchestrator: cleaning software tree %s", dir)
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("orchestrator: software cleanup %s: %v", dir, err)
			}
		}
	}()
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
		GitCloneURL:    cfg.GitCloneURL,
		UntarURL:       cfg.UntarURL,
	}
	o.broadcastRun(protocol.Envelope{Type: "command", Command: cmd})
	log.Printf("orchestrator: phase=%s timeout=%s", phase, timeout)

	return o.waitClientsIdle(ctx, timeout)
}

// sendFinalCleanup asks every participant to RemoveAll their host tree, waiting
// until idle so cleanup can finish after the paced final-delete window.
func (o *Orchestrator) sendFinalCleanup(ctx context.Context) error {
	cfg := o.Config()
	timeout := protocol.FinalCleanupTimeout
	o.setPhase(protocol.PhaseFinalDelete, 100, "Final Delete — cleaning remaining")

	cmd := &protocol.PhaseCommand{
		Phase:        protocol.PhaseFinalDelete,
		Percent:      100,
		Duration:     timeout.Seconds(),
		TestName:     cfg.TestName,
		ForceCleanup: true,
	}
	o.broadcastRun(protocol.Envelope{Type: "command", Command: cmd})
	log.Printf("orchestrator: final cleanup (force wipe) timeout=%s", timeout)

	return o.waitClientsIdle(ctx, timeout)
}

// waitClientsIdle waits until every run participant reports idle after starting work,
// or until timeout. Used for one-shot software phases that finish early.
func (o *Orchestrator) waitClientsIdle(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	sawRunning := false
	grace := time.Now().Add(1500 * time.Millisecond)

	o.mu.RLock()
	want := make(map[string]struct{}, len(o.runClientIDs))
	for _, id := range o.runClientIDs {
		want[id] = struct{}{}
	}
	o.mu.RUnlock()

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
			allIdle := true
			anyRunning := false
			seenWanted := 0
			for _, c := range clients {
				if _, ok := want[c.ID]; !ok {
					continue
				}
				seenWanted++
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
			// Missing participants are not "done" — OnClientRemoved aborts the run.
			if seenWanted < len(want) {
				continue
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
	o.broadcastRun(protocol.Envelope{Type: "command", Command: cmd})
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
