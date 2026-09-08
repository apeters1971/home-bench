package protocol

// LatencyBucketEdgesUs are upper bounds (µs) for IO op histograms (create/delete/read/write).
// The final count slot is the overflow bucket (> last edge).
var LatencyBucketEdgesUs = []float64{
	10, 20, 50, 100, 200, 500,
	1_000, 2_000, 5_000, 10_000, 20_000, 50_000,
	100_000, 200_000, 500_000,
	1_000_000, 2_000_000, 5_000_000, 10_000_000,
}

// LatencyLongBucketEdgesUs covers multi-second software phases (cold/warm/git/untar),
// roughly 0.5s–60s so p95/p99 land in real buckets instead of "> 10s" overflow.
var LatencyLongBucketEdgesUs = []float64{
	500_000,       // 0.5s
	1_000_000,     // 1s
	1_500_000,     // 1.5s
	2_000_000,     // 2s
	3_000_000,     // 3s
	4_000_000,     // 4s
	5_000_000,     // 5s
	7_500_000,     // 7.5s
	10_000_000,    // 10s
	12_500_000,    // 12.5s
	15_000_000,    // 15s
	20_000_000,    // 20s
	25_000_000,    // 25s
	30_000_000,    // 30s
	40_000_000,    // 40s
	50_000_000,    // 50s
	60_000_000,    // 60s
}

// LatencyScale selects which fixed edge table a histogram uses.
type LatencyScale int

const (
	LatencyScaleIO LatencyScale = iota
	LatencyScaleLong
)

// LatencyEdgesFor returns the upper-bound edges for a scale.
func LatencyEdgesFor(scale LatencyScale) []float64 {
	if scale == LatencyScaleLong {
		return LatencyLongBucketEdgesUs
	}
	return LatencyBucketEdgesUs
}

// LatencyBucketCount is len(edges)+1 for the IO scale (includes overflow).
func LatencyBucketCount() int {
	return len(LatencyBucketEdgesUs) + 1
}

// LatencyBucketCountFor is len(edges)+1 for the given scale (includes overflow).
func LatencyBucketCountFor(scale LatencyScale) int {
	return len(LatencyEdgesFor(scale)) + 1
}

// LatencyHistogram is a fixed-bucket distribution of operation latencies.
type LatencyHistogram struct {
	Counts   []uint64     `json:"counts"` // len = edges+1 for Scale
	Total    uint64       `json:"total"`  // successful observes only
	SumUs    float64      `json:"sum_us"`
	Failures uint64       `json:"failures"` // failed ops (not in Counts/Total/SumUs)
	Scale    LatencyScale `json:"scale,omitempty"`
}

// NewLatencyHistogram returns an empty IO-scale histogram.
func NewLatencyHistogram() LatencyHistogram {
	return NewLatencyHistogramScale(LatencyScaleIO)
}

// NewLatencyHistogramScale returns an empty histogram for the given scale.
func NewLatencyHistogramScale(scale LatencyScale) LatencyHistogram {
	return LatencyHistogram{
		Scale:  scale,
		Counts: make([]uint64, LatencyBucketCountFor(scale)),
	}
}

func (h *LatencyHistogram) ensureCounts() {
	n := LatencyBucketCountFor(h.Scale)
	if h.Counts == nil || len(h.Counts) != n {
		h.Counts = make([]uint64, n)
	}
}

// ObserveUs adds one successful sample (microseconds) into the histogram.
func (h *LatencyHistogram) ObserveUs(us float64) {
	h.ensureCounts()
	if us < 0 {
		us = 0
	}
	h.Total++
	h.SumUs += us
	edges := LatencyEdgesFor(h.Scale)
	for i, edge := range edges {
		if us <= edge {
			h.Counts[i]++
			return
		}
	}
	h.Counts[len(edges)]++
}

// ObserveFailure records a failed operation (excluded from latency stats).
func (h *LatencyHistogram) ObserveFailure() {
	h.Failures++
}

// Merge adds counts from another histogram (deltas or snapshots).
func (h *LatencyHistogram) Merge(other LatencyHistogram) {
	if other.Scale != 0 || h.Scale != 0 {
		// Prefer the non-zero scale if one side was zero-valued in JSON.
		if h.Scale == LatencyScaleIO && other.Scale != LatencyScaleIO {
			h.Scale = other.Scale
		}
	}
	h.ensureCounts()
	h.Failures += other.Failures
	if len(other.Counts) == 0 {
		h.Total += other.Total
		h.SumUs += other.SumUs
		return
	}
	n := len(h.Counts)
	if len(other.Counts) < n {
		n = len(other.Counts)
	}
	for i := 0; i < n; i++ {
		h.Counts[i] += other.Counts[i]
	}
	h.Total += other.Total
	h.SumUs += other.SumUs
}

// Clone returns a deep copy.
func (h LatencyHistogram) Clone() LatencyHistogram {
	out := LatencyHistogram{
		Total:    h.Total,
		SumUs:    h.SumUs,
		Failures: h.Failures,
		Scale:    h.Scale,
		Counts:   make([]uint64, LatencyBucketCountFor(h.Scale)),
	}
	copy(out.Counts, h.Counts)
	return out
}

// LatencySet holds the operation latency histograms for a run.
type LatencySet struct {
	Create       LatencyHistogram `json:"create"`
	Delete       LatencyHistogram `json:"delete"`
	Write        LatencyHistogram `json:"write"`
	Read         LatencyHistogram `json:"read"`
	StartupCold  LatencyHistogram `json:"startup_cold"`
	StartupWarm  LatencyHistogram `json:"startup_warm"`
	GitClone     LatencyHistogram `json:"git_clone"`
	Untar        LatencyHistogram `json:"untar"`
}

func NewLatencySet() LatencySet {
	return LatencySet{
		Create:      NewLatencyHistogramScale(LatencyScaleIO),
		Delete:      NewLatencyHistogramScale(LatencyScaleIO),
		Write:       NewLatencyHistogramScale(LatencyScaleIO),
		Read:        NewLatencyHistogramScale(LatencyScaleIO),
		StartupCold: NewLatencyHistogramScale(LatencyScaleLong),
		StartupWarm: NewLatencyHistogramScale(LatencyScaleLong),
		GitClone:    NewLatencyHistogramScale(LatencyScaleLong),
		Untar:       NewLatencyHistogramScale(LatencyScaleLong),
	}
}

func (s *LatencySet) Merge(other LatencySet) {
	s.Create.Merge(other.Create)
	s.Delete.Merge(other.Delete)
	s.Write.Merge(other.Write)
	s.Read.Merge(other.Read)
	s.StartupCold.Merge(other.StartupCold)
	s.StartupWarm.Merge(other.StartupWarm)
	s.GitClone.Merge(other.GitClone)
	s.Untar.Merge(other.Untar)
}

func (s LatencySet) Clone() LatencySet {
	return LatencySet{
		Create:      s.Create.Clone(),
		Delete:      s.Delete.Clone(),
		Write:       s.Write.Clone(),
		Read:        s.Read.Clone(),
		StartupCold: s.StartupCold.Clone(),
		StartupWarm: s.StartupWarm.Clone(),
		GitClone:    s.GitClone.Clone(),
		Untar:       s.Untar.Clone(),
	}
}
