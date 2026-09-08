package controller

import (
	"testing"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

func TestMetricsSpreadMultiSecondInterval(t *testing.T) {
	m := &MetricsStore{
		history:   make([]protocol.AggregatedSample, 0, 32),
		buckets:   make(map[int64]*bucket),
		latencies: protocol.NewLatencySet(),
		recording: true,
	}

	sec := time.Now().Unix()
	// 1000 clients × 2.5 create/s with a 10s report window → 25 ops in the sample.
	// Spreading must put ~2.5 create/s into each of the 10 seconds, not only the last.
	m.apply(protocol.MetricSample{
		Timestamp:   time.Unix(sec, 0).UTC(),
		IntervalSec: 10,
		CreateOps:   25,
	})

	var sum float64
	var seconds int
	for _, h := range m.history {
		sum += h.CreateIOPS
		seconds++
		if h.CreateIOPS < 2.4 || h.CreateIOPS > 2.6 {
			t.Fatalf("history %s create=%.3f want ~2.5", h.Timestamp, h.CreateIOPS)
		}
	}
	if b := m.buckets[sec]; b != nil {
		sum += b.createOps
		seconds++
		if b.createOps < 2.4 || b.createOps > 2.6 {
			t.Fatalf("open bucket create=%.3f want ~2.5", b.createOps)
		}
	}
	if seconds != 10 {
		t.Fatalf("credited %d seconds, want 10", seconds)
	}
	if sum < 24.9 || sum > 25.1 {
		t.Fatalf("total credited create=%.3f want 25", sum)
	}
}

func TestMetricsSpreadPatchesFlushedHistory(t *testing.T) {
	m := &MetricsStore{
		history:   make([]protocol.AggregatedSample, 0, 32),
		buckets:   make(map[int64]*bucket),
		latencies: protocol.NewLatencySet(),
		recording: true,
	}

	sec := time.Now().Unix()
	// Flush empty seconds into history first (as the 1Hz ticker would).
	for s := sec - 10; s < sec; s++ {
		m.buckets[s] = &bucket{ts: time.Unix(s, 0).UTC()}
	}
	m.flushClosed(sec)

	m.apply(protocol.MetricSample{
		Timestamp:   time.Unix(sec, 0).UTC(),
		IntervalSec: 10,
		CreateOps:   100,
	})

	for _, h := range m.history {
		age := sec - h.Timestamp.Unix()
		if age < 1 || age > 9 {
			continue // open second is the bucket; window covers sec-1..sec-9 here
		}
		if h.CreateIOPS < 9.9 || h.CreateIOPS > 10.1 {
			t.Fatalf("patched history %s create=%.3f want 10", h.Timestamp, h.CreateIOPS)
		}
	}
}
