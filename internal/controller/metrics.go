package controller

import (
	"math"
	"sort"
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

	in   chan protocol.MetricSample
	quit chan struct{}
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
				now := time.Now().Unix()
				// Keep the timeline advancing even when clients report sparsely
				// (scaled metrics intervals) or briefly stall between phases.
				m.ensureSecondsThrough(now)
				m.flushClosed(now)
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

	// Multi-second report windows (large fleets) must spread ops across the
	// window. Crediting ops/interval into only the current second under-reports
	// by ~interval (e.g. 10s reports → chart ~10× too low).
	scale := sample.IntervalSec
	if scale < 1 {
		scale = 1
	}
	windows := int(math.Round(scale))
	if windows < 1 {
		windows = 1
	}
	if windows > 120 {
		windows = 120
	}

	sec := sample.Timestamp.Unix()
	if sec <= 0 {
		sec = time.Now().Unix()
	}

	div := float64(windows)
	readInc := float64(sample.ReadOps) / div
	writeInc := float64(sample.WriteOps) / div
	createInc := float64(sample.CreateOps) / div
	deleteInc := float64(sample.DeleteOps) / div
	readBytesInc := float64(sample.ReadBytes) / div
	writeBytesInc := float64(sample.WriteBytes) / div

	for i := 0; i < windows; i++ {
		m.creditSecond(sec-int64(i), readInc, writeInc, createInc, deleteInc, readBytesInc, writeBytesInc)
	}
	m.latencies.Merge(sample.Latencies)

	m.flushClosed(sec)
	m.trim()
}

// creditSecond adds a per-second rate slice to an open bucket or, if that
// second was already flushed, patches the matching history point.
func (m *MetricsStore) creditSecond(sec int64, readOps, writeOps, createOps, deleteOps, readBytes, writeBytes float64) {
	if b, ok := m.buckets[sec]; ok {
		b.readOps += readOps
		b.writeOps += writeOps
		b.createOps += createOps
		b.deleteOps += deleteOps
		b.readBytes += readBytes
		b.writeBytes += writeBytes
		return
	}
	for i := len(m.history) - 1; i >= 0; i-- {
		hs := m.history[i].Timestamp.Unix()
		if hs > sec {
			continue
		}
		if hs == sec {
			m.history[i].ReadIOPS += readOps
			m.history[i].WriteIOPS += writeOps + createOps
			m.history[i].CreateIOPS += createOps
			m.history[i].DeleteIOPS += deleteOps
			m.history[i].ReadBps += readBytes
			m.history[i].WriteBps += writeBytes
			return
		}
		break
	}
	b := &bucket{ts: time.Unix(sec, 0).UTC()}
	b.readOps = readOps
	b.writeOps = writeOps
	b.createOps = createOps
	b.deleteOps = deleteOps
	b.readBytes = readBytes
	b.writeBytes = writeBytes
	m.buckets[sec] = b
}

// ensureSecondsThrough creates empty buckets for every second after the last
// history point through now, so UI charts keep a continuous time axis.
func (m *MetricsStore) ensureSecondsThrough(now int64) {
	start := now
	if n := len(m.history); n > 0 {
		start = m.history[n-1].Timestamp.Unix() + 1
	}
	if start > now {
		return
	}
	// Bound catch-up so a long pause cannot create a huge burst of empty points.
	if now-start > 120 {
		start = now - 120
	}
	for sec := start; sec <= now; sec++ {
		if _, ok := m.buckets[sec]; ok {
			continue
		}
		m.buckets[sec] = &bucket{ts: time.Unix(sec, 0).UTC()}
	}
}

// flushClosed promotes buckets older than currentSec into history in time order.
func (m *MetricsStore) flushClosed(currentSec int64) {
	type item struct {
		sec int64
		b   *bucket
	}
	due := make([]item, 0, len(m.buckets))
	for sec, b := range m.buckets {
		if sec < currentSec {
			due = append(due, item{sec: sec, b: b})
		}
	}
	if len(due) == 0 {
		return
	}
	sort.Slice(due, func(i, j int) bool { return due[i].sec < due[j].sec })
	for _, it := range due {
		b := it.b
		m.history = append(m.history, protocol.AggregatedSample{
			Timestamp:  b.ts,
			ReadIOPS:   b.readOps,
			WriteIOPS:  b.writeOps + b.createOps,
			ReadBps:    b.readBytes,
			WriteBps:   b.writeBytes,
			CreateIOPS: b.createOps,
			DeleteIOPS: b.deleteOps,
		})
		delete(m.buckets, it.sec)
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
