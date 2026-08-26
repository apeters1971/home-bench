package protocol

import "time"

// Phase identifies the current test stage.
type Phase string

const (
	PhaseIdle        Phase = "idle"
	PhaseCreate      Phase = "create"
	PhaseDelete      Phase = "delete"
	PhaseWriteBW     Phase = "write_bw"
	PhaseReadBW      Phase = "read_bw"
	PhaseReadWrite   Phase = "read_write"
	PhaseFinalDelete Phase = "final_delete"
	PhaseStopped     Phase = "stopped"
)

// RampPercents is the fixed intensity ladder used by every phase.
var RampPercents = []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}

const (
	// BandwidthFileSize is the size written/read during bandwidth phases.
	BandwidthFileSize = 64 * 1024 * 1024 // 64 MiB
	// CreateFileSize is the size used during create/delete IOPS phases.
	CreateFileSize = 4096
	// MetricsInterval is how often clients push observations.
	MetricsInterval = time.Second
	// HistoryRetention is how long the controller keeps metric samples.
	HistoryRetention = 30 * time.Minute
	// DefaultPhaseStepDuration is the default time spent at each 10% ramp step.
	DefaultPhaseStepDuration = 30 * time.Second
	// WSPingInterval is how often ping frames are sent to keep NAT mappings alive.
	WSPingInterval = 20 * time.Second
	// WSReadTimeout is the idle read deadline; refreshed on pong/data.
	WSReadTimeout = 60 * time.Second
	// ClientStaleAfter removes registry entries with no touch for this long.
	ClientStaleAfter = 90 * time.Second
)

// Config is the centrally configured test parameters.
type Config struct {
	TestName           string   `json:"test_name"`
	Prefixes           []string `json:"prefixes"`
	FileCreationRate   float64  `json:"file_creation_rate"`   // files/sec global
	FileDeletionRate   float64  `json:"file_deletion_rate"`   // files/sec global
	FileWriteBandwidth float64  `json:"file_write_bandwidth"` // bytes/sec global
	FileReadBandwidth  float64  `json:"file_read_bandwidth"`  // bytes/sec global
	PhaseStepSeconds   float64  `json:"phase_step_seconds"`   // seconds at each 10% ramp step
}

// DefaultConfig returns sensible starting values.
func DefaultConfig() Config {
	return Config{
		TestName:           "bench1",
		Prefixes:           []string{"/tmp/homebench"},
		FileCreationRate:   1000,
		FileDeletionRate:   1000,
		FileWriteBandwidth: 500 * 1024 * 1024, // 500 MiB/s
		FileReadBandwidth:  500 * 1024 * 1024,
		PhaseStepSeconds:   DefaultPhaseStepDuration.Seconds(),
	}
}

// PhaseStepDuration returns the configured ramp-step length.
func (c Config) PhaseStepDuration() time.Duration {
	if c.PhaseStepSeconds <= 0 {
		return DefaultPhaseStepDuration
	}
	return time.Duration(c.PhaseStepSeconds * float64(time.Second))
}

// ClientInfo describes a registered client.
type ClientInfo struct {
	ID       string    `json:"id"`
	Hostname string    `json:"hostname"`
	Prefix   string    `json:"prefix"`
	LastSeen time.Time `json:"last_seen"`
	Status   string    `json:"status"`
	Phase    Phase     `json:"phase"`
}

// MetricSample is one second of observed IO from a client.
type MetricSample struct {
	ClientID   string    `json:"client_id"`
	Timestamp  time.Time `json:"timestamp"`
	Phase      Phase     `json:"phase"`
	Percent    int       `json:"percent"`
	ReadOps    int64     `json:"read_ops"`
	WriteOps   int64     `json:"write_ops"`
	DeleteOps  int64     `json:"delete_ops"`
	CreateOps  int64     `json:"create_ops"`
	ReadBytes  int64     `json:"read_bytes"`
	WriteBytes int64     `json:"write_bytes"`
	// Latency deltas since the previous metrics push (bucket counts).
	Latencies LatencySet `json:"latencies"`
}

// AggregatedSample is the controller-side sum across clients for one second.
type AggregatedSample struct {
	Timestamp  time.Time `json:"timestamp"`
	ReadIOPS   float64   `json:"read_iops"`
	WriteIOPS  float64   `json:"write_iops"`
	ReadBps    float64   `json:"read_bps"`
	WriteBps   float64   `json:"write_bps"`
	CreateIOPS float64   `json:"create_iops"`
	DeleteIOPS float64   `json:"delete_iops"`
}

// PhaseCommand tells clients what to execute.
type PhaseCommand struct {
	Phase      Phase   `json:"phase"`
	Percent    int     `json:"percent"`
	Duration   float64 `json:"duration_sec"` // 0 = run until complete / until stop
	Rate       float64 `json:"rate"`         // ops/sec or bytes/sec depending on phase
	ReadRate   float64 `json:"read_rate"`    // bytes/sec for read side of read_write
	TestName   string  `json:"test_name"`
	Prefix     string  `json:"prefix"`
	FileSize   int64   `json:"file_size"`
	StartIndex int64   `json:"start_index"` // for create: starting file index
	CountHint  int64   `json:"count_hint"`  // expected files for delete/bw phases
}

// Envelope is the WebSocket / HTTP message wrapper.
type Envelope struct {
	Type string `json:"type"`
	// Client -> Controller
	Register *RegisterMsg `json:"register,omitempty"`
	Metrics  *MetricSample `json:"metrics,omitempty"`
	Status   *StatusMsg    `json:"status,omitempty"`
	// Controller -> Client
	Welcome *WelcomeMsg   `json:"welcome,omitempty"`
	Command *PhaseCommand `json:"command,omitempty"`
	Stop    *StopMsg      `json:"stop,omitempty"`
	Config  *Config       `json:"config,omitempty"`
}

type RegisterMsg struct {
	Hostname string `json:"hostname"`
	ID       string `json:"id,omitempty"`
}

type WelcomeMsg struct {
	ClientID string `json:"client_id"`
	Prefix   string `json:"prefix"`
	Config   Config `json:"config"`
}

type StatusMsg struct {
	ClientID string `json:"client_id"`
	Status   string `json:"status"`
	Phase    Phase  `json:"phase"`
	Percent  int    `json:"percent"`
	Detail   string `json:"detail,omitempty"`
	Files    int64  `json:"files,omitempty"`
}

type StopMsg struct {
	Cleanup bool `json:"cleanup"`
}

// PhaseSpan marks when a major test phase was active (for chart overlays).
type PhaseSpan struct {
	Phase Phase      `json:"phase"`
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"` // nil = still open
}

// UIState is the snapshot served to the Web UI.
type UIState struct {
	Config      Config             `json:"config"`
	Clients     []ClientInfo       `json:"clients"`
	Running     bool               `json:"running"`
	Phase       Phase              `json:"phase"`
	Percent     int                `json:"percent"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	ElapsedSec  float64            `json:"elapsed_sec"`
	History         []AggregatedSample `json:"history"`
	Latencies       LatencySet         `json:"latencies"`
	LatencyEdgesUs  []float64          `json:"latency_edges_us"`
	PhaseSpans      []PhaseSpan        `json:"phase_spans"`
	PhaseOrder      []Phase            `json:"phase_order"`
	StatusText      string             `json:"status_text"`
	ClientCount     int                `json:"client_count"`
}

// PhaseOrder is the fixed sequence of a full run.
var PhaseOrder = []Phase{
	PhaseCreate,
	PhaseDelete,
	PhaseWriteBW,
	PhaseReadBW,
	PhaseReadWrite,
	PhaseFinalDelete,
}

func PhaseLabel(p Phase) string {
	switch p {
	case PhaseIdle:
		return "Idle"
	case PhaseCreate:
		return "Create"
	case PhaseDelete:
		return "Delete"
	case PhaseWriteBW:
		return "Write BW"
	case PhaseReadBW:
		return "Read BW"
	case PhaseReadWrite:
		return "Read+Write"
	case PhaseFinalDelete:
		return "Final Delete"
	case PhaseStopped:
		return "Stopped"
	default:
		return string(p)
	}
}
