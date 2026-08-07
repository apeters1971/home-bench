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
	mu     sync.Mutex
	indices []int64
	next   int64
	delPos int // persistent cursor across delete ramp steps
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

func (l *FileLedger) ResetDeleteCursor() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.delPos = 0
}

func (l *FileLedger) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.indices = l.indices[:0]
	l.next = 0
	l.delPos = 0
}

// NextExistingPath finds the next ledger path that still exists.
// removeFromLedger=true pops it (final delete); false only advances the cursor
// so mid-test delete can keep paths for later bandwidth phases.
func (l *FileLedger) NextExistingPath(prefix, testName, hostname string, removeFromLedger bool) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.indices) == 0 {
		return "", false
	}

	scanned := 0
	for scanned < len(l.indices) {
		if l.delPos >= len(l.indices) {
			if removeFromLedger {
				return "", false
			}
			// One wrap to catch leftovers after a partial first pass.
			l.delPos = 0
		}
		idx := l.indices[l.delPos]
		path := FilePath(prefix, testName, hostname, idx)
		if _, err := os.Stat(path); err != nil {
			// Missing — drop stale entries during final delete; skip during mid-delete.
			if removeFromLedger {
				l.indices = append(l.indices[:l.delPos], l.indices[l.delPos+1:]...)
			} else {
				l.delPos++
			}
			scanned++
			continue
		}
		if removeFromLedger {
			l.indices = append(l.indices[:l.delPos], l.indices[l.delPos+1:]...)
		} else {
			l.delPos++
		}
		return path, true
	}
	return "", false
}

// Worker executes a single phase command until duration elapses or ctx cancels.
type Worker struct {
	Hostname string
	Prefix   string
	Ledger   *FileLedger
	Stats    *Stats
}

func NewWorker(hostname, prefix string, ledger *FileLedger, stats *Stats) *Worker {
	return &Worker{
		Hostname: hostname,
		Prefix:   prefix,
		Ledger:   ledger,
		Stats:    stats,
	}
}

func (w *Worker) Run(ctx context.Context, cmd protocol.PhaseCommand) error {
	switch cmd.Phase {
	case protocol.PhaseCreate:
		w.Ledger.ResetDeleteCursor()
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
	removeFromLedger := cmd.Phase == protocol.PhaseFinalDelete

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

		path, ok := w.Ledger.NextExistingPath(cmd.Prefix, cmd.TestName, w.Hostname, removeFromLedger)
		if !ok {
			path, ok = w.nextOnDisk(cmd)
			if !ok {
				// Nothing left — wait out the step so the ramp stays aligned.
				if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		}
		if err := os.Remove(path); err != nil {
			continue
		}
		w.Stats.DeleteOps.Add(1)
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
		for i := int64(0); i < 64; i++ {
			idx := w.Ledger.NextIndex()
			w.Ledger.Add(idx)
		}
		indices = w.Ledger.Snapshot()
	}
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
	// O_DIRECT requires aligned buffers and transfer sizes.
	buf := alignedBuffer(1024 * 1024)
	if fileSize%int64(directAlign) != 0 {
		fileSize = (fileSize / int64(directAlign)) * int64(directAlign)
		if fileSize == 0 {
			fileSize = int64(directAlign)
		}
	}

	if doWrite {
		_, _ = rand.Read(buf[:min(len(buf), 4096)])
	}

	var (
		cursor      int
		transferred float64
		start       = time.Now()
		file        *os.File
		fileOff     int64
		writing     bool
	)
	closeFile := func() {
		if file != nil {
			_ = file.Close()
			file = nil
		}
		fileOff = 0
		writing = false
	}
	defer closeFile()

	openNext := func() error {
		closeFile()
		idx := indices[cursor%len(indices)]
		cursor++
		path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
		if doWrite {
			if err := EnsureParent(path); err != nil {
				return err
			}
			f, err := openDirectWrite(path)
			if err != nil {
				// Fallback if the FS rejects O_DIRECT.
				f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
				if err != nil {
					return err
				}
			}
			file = f
			writing = true
			return nil
		}
		f, err := openDirectRead(path)
		if err != nil {
			// File may not exist yet — seed with Direct I/O, then reopen for read.
			if err := EnsureParent(path); err != nil {
				return err
			}
			if err := writeFileDirect(path, buf, fileSize); err != nil {
				return err
			}
			f, err = openDirectRead(path)
			if err != nil {
				f, err = os.Open(path)
				if err != nil {
					return err
				}
			}
		}
		file = f
		writing = false
		return nil
	}

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
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		if transferred >= bytesPerSec*elapsed {
			if err := sleepCtx(ctx, time.Millisecond); err != nil {
				return err
			}
			continue
		}

		if file == nil {
			if err := openNext(); err != nil {
				if err := sleepCtx(ctx, 5*time.Millisecond); err != nil {
					return err
				}
				continue
			}
		}

		chunk := int64(len(buf))
		if writing {
			remain := fileSize - fileOff
			if remain <= 0 {
				w.Stats.WriteOps.Add(1)
				closeFile()
				continue
			}
			if chunk > remain {
				chunk = remain
			}
			// Keep Direct I/O transfers aligned.
			if chunk%int64(directAlign) != 0 {
				chunk = (chunk / int64(directAlign)) * int64(directAlign)
				if chunk == 0 {
					closeFile()
					continue
				}
			}
			n, err := file.Write(buf[:chunk])
			if n > 0 {
				fileOff += int64(n)
				transferred += float64(n)
				w.Stats.WriteBytes.Add(int64(n))
			}
			if err != nil {
				closeFile()
				continue
			}
			if fileOff >= fileSize {
				w.Stats.WriteOps.Add(1)
				closeFile()
			}
			continue
		}

		// Read path — Direct I/O streamed in aligned chunks.
		remain := fileSize - fileOff
		if remain <= 0 {
			if fileOff > 0 {
				w.Stats.ReadOps.Add(1)
			}
			closeFile()
			continue
		}
		if chunk > remain {
			chunk = remain
		}
		if chunk%int64(directAlign) != 0 {
			chunk = (chunk / int64(directAlign)) * int64(directAlign)
			if chunk == 0 {
				if fileOff > 0 {
					w.Stats.ReadOps.Add(1)
				}
				closeFile()
				continue
			}
		}
		n, err := file.Read(buf[:chunk])
		if n > 0 {
			transferred += float64(n)
			w.Stats.ReadBytes.Add(int64(n))
			fileOff += int64(n)
		}
		if err == io.EOF || fileOff >= fileSize {
			if fileOff > 0 {
				w.Stats.ReadOps.Add(1)
			}
			closeFile()
			continue
		}
		if err != nil {
			closeFile()
			continue
		}
	}
}

func writeFileDirect(path string, buf []byte, size int64) error {
	f, err := openDirectWrite(path)
	if err != nil {
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
	}
	defer f.Close()
	var written int64
	for written < size {
		chunk := int64(len(buf))
		if size-written < chunk {
			chunk = size - written
		}
		if chunk%int64(directAlign) != 0 {
			chunk = (chunk / int64(directAlign)) * int64(directAlign)
			if chunk == 0 {
				break
			}
		}
		n, err := f.Write(buf[:chunk])
		written += int64(n)
		if err != nil {
			return err
		}
	}
	return nil
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
		return protocol.DefaultPhaseStepDuration
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
