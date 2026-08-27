package protocol

// LatencyBucketEdgesUs are upper bounds (µs) for fixed latency histogram buckets.
// The final count slot is the overflow bucket (> last edge).
var LatencyBucketEdgesUs = []float64{
	10, 20, 50, 100, 200, 500,
	1_000, 2_000, 5_000, 10_000, 20_000, 50_000,
	100_000, 200_000, 500_000,
	1_000_000, 2_000_000, 5_000_000, 10_000_000,
}

// LatencyBucketCount is len(edges)+1 (includes overflow).
func LatencyBucketCount() int {
	return len(LatencyBucketEdgesUs) + 1
}

// LatencyHistogram is a fixed-bucket distribution of operation latencies.
type LatencyHistogram struct {
	Counts []uint64 `json:"counts"` // len = LatencyBucketCount()
	Total  uint64   `json:"total"`
	SumUs  float64  `json:"sum_us"`
}

// NewLatencyHistogram returns an empty histogram with allocated buckets.
func NewLatencyHistogram() LatencyHistogram {
	return LatencyHistogram{Counts: make([]uint64, LatencyBucketCount())}
}

// ObserveUs adds one sample (microseconds) into the histogram.
func (h *LatencyHistogram) ObserveUs(us float64) {
	if h.Counts == nil || len(h.Counts) != LatencyBucketCount() {
		h.Counts = make([]uint64, LatencyBucketCount())
	}
	if us < 0 {
		us = 0
	}
	h.Total++
	h.SumUs += us
	for i, edge := range LatencyBucketEdgesUs {
		if us <= edge {
			h.Counts[i]++
			return
		}
	}
	h.Counts[len(LatencyBucketEdgesUs)]++
}

// Merge adds counts from another histogram (deltas or snapshots).
func (h *LatencyHistogram) Merge(other LatencyHistogram) {
	if h.Counts == nil || len(h.Counts) != LatencyBucketCount() {
		h.Counts = make([]uint64, LatencyBucketCount())
	}
	if len(other.Counts) == 0 {
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
		Total: h.Total,
		SumUs: h.SumUs,
		Counts: make([]uint64, LatencyBucketCount()),
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
		Create:      NewLatencyHistogram(),
		Delete:      NewLatencyHistogram(),
		Write:       NewLatencyHistogram(),
		Read:        NewLatencyHistogram(),
		StartupCold: NewLatencyHistogram(),
		StartupWarm: NewLatencyHistogram(),
		GitClone:    NewLatencyHistogram(),
		Untar:       NewLatencyHistogram(),
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
