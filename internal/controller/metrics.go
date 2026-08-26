package controller

import (
	"sync"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// MetricsStore aggregates per-second client samples into a rolling history.
// Recording is active only while a test run is in progress; afterward the
// history is frozen until the next Start.
type MetricsStore struct {
	mu        sync.RWMutex
	recording bool
	history   []protocol.AggregatedSample
	// pending buckets keyed by unix second
	buckets   map[int64]*bucket
	latencies protocol.LatencySet
}

type bucket struct {
	ts         time.Time
	readOps    int64
	writeOps   int64
	createOps  int64
	deleteOps  int64
	readBytes  int64
	writeBytes int64
	clients    map[string]struct{}
}

func NewMetricsStore() *MetricsStore {
	return &MetricsStore{
		history:   make([]protocol.AggregatedSample, 0, 2048),
		buckets:   make(map[int64]*bucket),
		latencies: protocol.NewLatencySet(),
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
	// Flush every open bucket, including the current second.
	m.flushClosed(time.Now().Unix() + 1)
}

func (m *MetricsStore) Recording() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recording
}

func (m *MetricsStore) Add(sample protocol.MetricSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.recording {
		return
	}

	sec := sample.Timestamp.Unix()
	b, ok := m.buckets[sec]
	if !ok {
		b = &bucket{
			ts:      time.Unix(sec, 0).UTC(),
			clients: make(map[string]struct{}),
		}
		m.buckets[sec] = b
	}
	b.readOps += sample.ReadOps
	b.writeOps += sample.WriteOps
	b.createOps += sample.CreateOps
	b.deleteOps += sample.DeleteOps
	b.readBytes += sample.ReadBytes
	b.writeBytes += sample.WriteBytes
	b.clients[sample.ClientID] = struct{}{}
	m.latencies.Merge(sample.Latencies)

	m.flushClosed(sec)
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
			ReadIOPS:   float64(b.readOps),
			WriteIOPS:  float64(b.writeOps + b.createOps),
			ReadBps:    float64(b.readBytes),
			WriteBps:   float64(b.writeBytes),
			CreateIOPS: float64(b.createOps),
			DeleteIOPS: float64(b.deleteOps),
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recording {
		m.flushClosed(time.Now().Unix())
		m.trim()
	}
	out := make([]protocol.AggregatedSample, len(m.history))
	copy(out, m.history)
	// Include the in-progress second only while a run is recording.
	if m.recording {
		sec := time.Now().Unix()
		if b, ok := m.buckets[sec]; ok {
			out = append(out, protocol.AggregatedSample{
				Timestamp:  b.ts,
				ReadIOPS:   float64(b.readOps),
				WriteIOPS:  float64(b.writeOps + b.createOps),
				ReadBps:    float64(b.readBytes),
				WriteBps:   float64(b.writeBytes),
				CreateIOPS: float64(b.createOps),
				DeleteIOPS: float64(b.deleteOps),
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
