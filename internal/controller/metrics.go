package controller

import (
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// MetricsStore aggregates per-second client samples into a rolling history.
// Recording is active only while a test run is in progress; afterward the
// history is frozen until the next Start.
//
// Samples are ingested via a buffered channel and applied by a single goroutine
// so thousands of client WS readers do not contend on one mutex.
type MetricsStore struct {
	mu        sync.RWMutex
	recording bool
	history   []protocol.AggregatedSample
	buckets   map[int64]*bucket
	latencies protocol.LatencySet

	in     chan protocol.MetricSample
	quit   chan struct{}
	closed sync.Once
}

type bucket struct {
	ts         time.Time
	readOps    float64
	writeOps   float64
	createOps  float64
	deleteOps  float64
	readBytes  float64
	writeBytes float64
}

func NewMetricsStore() *MetricsStore {
	m := &MetricsStore{
		history:   make([]protocol.AggregatedSample, 0, 2048),
		buckets:   make(map[int64]*bucket),
		latencies: protocol.NewLatencySet(),
		in:        make(chan protocol.MetricSample, 16384),
		quit:      make(chan struct{}),
	}
	go m.loop()
	return m
}

func (m *MetricsStore) loop() {
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	for {
		select {
		case <-m.quit:
			return
		case sample := <-m.in:
			m.apply(sample)
		case <-flush.C:
			m.mu.Lock()
			if m.recording {
				m.flushClosed(time.Now().Unix())
				m.trim()
			}
			m.mu.Unlock()
		}
	}
}

func (m *MetricsStore) Begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = m.history[:0]
	m.buckets = make(map[int64]*bucket)
	m.latencies = protocol.NewLatencySet()
	m.recording = true
}

// Freeze stops accepting samples and seals open buckets into history.
func (m *MetricsStore) Freeze() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recording = false
	m.flushClosed(time.Now().Unix() + 1)
}

func (m *MetricsStore) Recording() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recording
}

// Add enqueues a sample for aggregation. Non-blocking: under extreme overload
// the newest sample is dropped rather than stalling the client WS reader.
func (m *MetricsStore) Add(sample protocol.MetricSample) {
	select {
	case m.in <- sample:
	default:
		// Drop rather than block thousands of readers.
	}
}

func (m *MetricsStore) apply(sample protocol.MetricSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.recording {
		return
	}

	// Normalize multi-second report windows to per-second rates and spread
	// them across the covered seconds so chart magnitudes stay correct.
	scale := sample.IntervalSec
	if scale < 1 {
		scale = 1
	}
	nSec := int(scale + 0.5)
	if nSec < 1 {
		nSec = 1
	}
	readOps := float64(sample.ReadOps) / scale
	writeOps := float64(sample.WriteOps) / scale
	createOps := float64(sample.CreateOps) / scale
	deleteOps := float64(sample.DeleteOps) / scale
	readBytes := float64(sample.ReadBytes) / scale
	writeBytes := float64(sample.WriteBytes) / scale

	end := sample.Timestamp.Unix()
	for i := 0; i < nSec; i++ {
		sec := end - int64(i)
		b, ok := m.buckets[sec]
		if !ok {
			b = &bucket{ts: time.Unix(sec, 0).UTC()}
			m.buckets[sec] = b
		}
		b.readOps += readOps
		b.writeOps += writeOps
		b.createOps += createOps
		b.deleteOps += deleteOps
		b.readBytes += readBytes
		b.writeBytes += writeBytes
	}
	m.latencies.Merge(sample.Latencies)

	m.flushClosed(end)
	m.trim()
}

// flushClosed promotes buckets older than currentSec into history.
func (m *MetricsStore) flushClosed(currentSec int64) {
	for sec, b := range m.buckets {
		if sec >= currentSec {
			continue
		}
		m.history = append(m.history, protocol.AggregatedSample{
			Timestamp:  b.ts,
			ReadIOPS:   b.readOps,
			WriteIOPS:  b.writeOps + b.createOps,
			ReadBps:    b.readBytes,
			WriteBps:   b.writeBytes,
			CreateIOPS: b.createOps,
			DeleteIOPS: b.deleteOps,
		})
		delete(m.buckets, sec)
	}
}

func (m *MetricsStore) trim() {
	cutoff := time.Now().Add(-protocol.HistoryRetention)
	i := 0
	for i < len(m.history) && m.history[i].Timestamp.Before(cutoff) {
		i++
	}
	if i > 0 {
		m.history = append([]protocol.AggregatedSample{}, m.history[i:]...)
	}
}

func (m *MetricsStore) History() []protocol.AggregatedSample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]protocol.AggregatedSample, len(m.history))
	copy(out, m.history)
	if m.recording {
		sec := time.Now().Unix()
		if b, ok := m.buckets[sec]; ok {
			out = append(out, protocol.AggregatedSample{
				Timestamp:  b.ts,
				ReadIOPS:   b.readOps,
				WriteIOPS:  b.writeOps + b.createOps,
				ReadBps:    b.readBytes,
				WriteBps:   b.writeBytes,
				CreateIOPS: b.createOps,
				DeleteIOPS: b.deleteOps,
			})
		}
	}
	return out
}

func (m *MetricsStore) Latencies() protocol.LatencySet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latencies.Clone()
}
