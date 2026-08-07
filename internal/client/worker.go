package client

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

// Stats are atomically updated counters flushed each metrics interval.
type Stats struct {
	ReadOps    atomic.Int64
	WriteOps   atomic.Int64
	CreateOps  atomic.Int64
	DeleteOps  atomic.Int64
	ReadBytes  atomic.Int64
	WriteBytes atomic.Int64
}

func (s *Stats) SnapshotAndReset() protocol.MetricSample {
	return protocol.MetricSample{
		ReadOps:    s.ReadOps.Swap(0),
		WriteOps:   s.WriteOps.Swap(0),
		CreateOps:  s.CreateOps.Swap(0),
		DeleteOps:  s.DeleteOps.Swap(0),
		ReadBytes:  s.ReadBytes.Swap(0),
		WriteBytes: s.WriteBytes.Swap(0),
		Timestamp:  time.Now().UTC(),
	}
}

// FileLedger tracks files this client created so delete/bw/read can target them.
type FileLedger struct {
	mu      sync.Mutex
	indices []int64
	next    int64
}

func NewFileLedger() *FileLedger {
	return &FileLedger{indices: make([]int64, 0, 4096)}
}

func (l *FileLedger) Add(idx int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.indices = append(l.indices, idx)
	if idx >= l.next {
		l.next = idx + 1
	}
}

func (l *FileLedger) NextIndex() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.next
	l.next++
	return idx
}

func (l *FileLedger) Count() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.indices))
}

func (l *FileLedger) Snapshot() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int64, len(l.indices))
	copy(out, l.indices)
	return out
}

func (l *FileLedger) Pop() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.indices) == 0 {
		return 0, false
	}
	idx := l.indices[len(l.indices)-1]
	l.indices = l.indices[:len(l.indices)-1]
	return idx, true
}

func (l *FileLedger) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.indices = l.indices[:0]
	l.next = 0
}

// Worker executes a single phase command until duration elapses or ctx cancels.
type Worker struct {
	Hostname string
	Prefix   string
	Ledger   *FileLedger
	Stats    *Stats
	bufPool  sync.Pool
}

func NewWorker(hostname, prefix string, ledger *FileLedger, stats *Stats) *Worker {
	return &Worker{
		Hostname: hostname,
		Prefix:   prefix,
		Ledger:   ledger,
		Stats:    stats,
		bufPool: sync.Pool{New: func() any {
			b := make([]byte, 1024*1024)
			return &b
		}},
	}
}

func (w *Worker) Run(ctx context.Context, cmd protocol.PhaseCommand) error {
	switch cmd.Phase {
	case protocol.PhaseCreate:
		return w.runCreate(ctx, cmd)
	case protocol.PhaseDelete, protocol.PhaseFinalDelete:
		return w.runDelete(ctx, cmd)
	case protocol.PhaseWriteBW:
		return w.runWriteBW(ctx, cmd)
	case protocol.PhaseReadBW:
		return w.runReadBW(ctx, cmd)
	case protocol.PhaseReadWrite:
		return w.runReadWrite(ctx, cmd)
	default:
		return fmt.Errorf("unknown phase %s", cmd.Phase)
	}
}

func (w *Worker) runCreate(ctx context.Context, cmd protocol.PhaseCommand) error {
	if cmd.Rate <= 0 {
		return sleepCtx(ctx, durationOf(cmd))
	}
	deadline := time.Now().Add(durationOf(cmd))
	payload := make([]byte, cmd.FileSize)
	_, _ = rand.Read(payload[:min(64, len(payload))])

	var created float64
	start := time.Now()
	for {
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		elapsed := time.Since(start).Seconds()
		target := cmd.Rate * elapsed
		if created >= target {
			if err := sleepCtx(ctx, time.Millisecond); err != nil {
				return err
			}
			continue
		}
		idx := w.Ledger.NextIndex()
		path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
		if err := EnsureParent(path); err != nil {
			continue
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			continue
		}
		w.Ledger.Add(idx)
		w.Stats.CreateOps.Add(1)
		w.Stats.WriteBytes.Add(int64(len(payload)))
		created++
	}
}

func (w *Worker) runDelete(ctx context.Context, cmd protocol.PhaseCommand) error {
	if cmd.Rate <= 0 {
		return sleepCtx(ctx, durationOf(cmd))
	}
	deadline := time.Now().Add(durationOf(cmd))

	// Mid-test delete keeps the ledger so later bandwidth phases reuse the same paths.
	// Final delete clears the ledger as files are removed.
	keepLedger := cmd.Phase == protocol.PhaseDelete
	indices := w.Ledger.Snapshot()
	pos := len(indices) - 1

	var deleted float64
	start := time.Now()
	for {
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		elapsed := time.Since(start).Seconds()
		target := cmd.Rate * elapsed
		if deleted >= target {
			if err := sleepCtx(ctx, time.Millisecond); err != nil {
				return err
			}
			continue
		}

		var path string
		if keepLedger {
			if pos < 0 {
				p, ok := w.nextOnDisk(cmd)
				if !ok {
					return nil
				}
				path = p
			} else {
				path = FilePath(cmd.Prefix, cmd.TestName, w.Hostname, indices[pos])
				pos--
			}
		} else {
			idx, ok := w.Ledger.Pop()
			if !ok {
				p, ok := w.nextOnDisk(cmd)
				if !ok {
					return nil
				}
				path = p
			} else {
				path = FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
			}
		}
		if err := os.Remove(path); err == nil {
			w.Stats.DeleteOps.Add(1)
		}
		deleted++
	}
}

func (w *Worker) nextOnDisk(cmd protocol.PhaseCommand) (string, bool) {
	root := HostRoot(cmd.Prefix, cmd.TestName, w.Hostname)
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		found = path
		return io.EOF
	})
	return found, found != ""
}

func (w *Worker) runWriteBW(ctx context.Context, cmd protocol.PhaseCommand) error {
	return w.runBandwidth(ctx, cmd, true, false)
}

func (w *Worker) runReadBW(ctx context.Context, cmd protocol.PhaseCommand) error {
	return w.runBandwidth(ctx, cmd, false, true)
}

func (w *Worker) runReadWrite(ctx context.Context, cmd protocol.PhaseCommand) error {
	writeCmd := cmd
	readCmd := cmd
	readCmd.Rate = cmd.ReadRate
	if readCmd.Rate <= 0 {
		readCmd.Rate = cmd.Rate
	}
	errCh := make(chan error, 2)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { errCh <- w.runBandwidth(cctx, writeCmd, true, false) }()
	go func() { errCh <- w.runBandwidth(cctx, readCmd, false, true) }()
	var first error
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	if first == context.Canceled || first == context.DeadlineExceeded {
		return nil
	}
	return first
}

func (w *Worker) runBandwidth(ctx context.Context, cmd protocol.PhaseCommand, doWrite, doRead bool) error {
	indices := w.Ledger.Snapshot()
	if len(indices) == 0 {
		// Seed a modest working set if create phase produced no ledger entries.
		for i := int64(0); i < 64; i++ {
			idx := w.Ledger.NextIndex()
			w.Ledger.Add(idx)
		}
		indices = w.Ledger.Snapshot()
	}
	// Cap working set so 64 MiB rewrites cannot fill the filesystem unboundedly.
	const maxBWFiles = 256
	if len(indices) > maxBWFiles {
		indices = indices[:maxBWFiles]
	}

	fileSize := cmd.FileSize
	if fileSize <= 0 {
		fileSize = protocol.BandwidthFileSize
	}
	bytesPerSec := cmd.Rate
	if bytesPerSec <= 0 {
		return sleepCtx(ctx, durationOf(cmd))
	}

	deadline := time.Now().Add(durationOf(cmd))
	bufPtr := w.bufPool.Get().(*[]byte)
	buf := *bufPtr
	defer w.bufPool.Put(bufPtr)

	// Fill buffer once for writes.
	if doWrite {
		_, _ = rand.Read(buf[:min(len(buf), 4096)])
	}

	var cursor int
	var bytesThisSec int64
	secStart := time.Now()

	for {
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Token-bucket style throttle per second.
		if time.Since(secStart) >= time.Second {
			secStart = time.Now()
			bytesThisSec = 0
		}
		if float64(bytesThisSec) >= bytesPerSec {
			sleepCtx(ctx, 5*time.Millisecond)
			continue
		}

		idx := indices[cursor%len(indices)]
		cursor++
		path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)

		if doWrite {
			if err := EnsureParent(path); err != nil {
				continue
			}
			n, err := writeFileSized(path, buf, fileSize)
			if err == nil {
				w.Stats.WriteOps.Add(1)
				w.Stats.WriteBytes.Add(n)
				bytesThisSec += n
			}
		}
		if doRead {
			n, err := readFileFull(path, buf)
			if err == nil {
				w.Stats.ReadOps.Add(1)
				w.Stats.ReadBytes.Add(n)
				bytesThisSec += n
			}
		}
	}
}

func writeFileSized(path string, buf []byte, size int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var written int64
	for written < size {
		chunk := int64(len(buf))
		if size-written < chunk {
			chunk = size - written
		}
		n, err := f.Write(buf[:chunk])
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, f.Sync()
}

func readFileFull(path string, buf []byte) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var total int64
	for {
		n, err := f.Read(buf)
		total += int64(n)
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Cleanup removes all files under this client's host root for the test.
func (w *Worker) Cleanup(prefix, testName string) error {
	root := HostRoot(prefix, testName, w.Hostname)
	err := os.RemoveAll(root)
	w.Ledger.Clear()
	return err
}

func durationOf(cmd protocol.PhaseCommand) time.Duration {
	if cmd.Duration <= 0 {
		return protocol.CreateStepDuration
	}
	return time.Duration(cmd.Duration * float64(time.Second))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
