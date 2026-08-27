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

	latMu     sync.Mutex
	latencies protocol.LatencySet
}

func NewStats() *Stats {
	return &Stats{latencies: protocol.NewLatencySet()}
}

func (s *Stats) ObserveCreate(d time.Duration) {
	s.latMu.Lock()
	s.latencies.Create.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveDelete(d time.Duration) {
	s.latMu.Lock()
	s.latencies.Delete.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveWrite(d time.Duration) {
	s.latMu.Lock()
	s.latencies.Write.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveRead(d time.Duration) {
	s.latMu.Lock()
	s.latencies.Read.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveStartupCold(d time.Duration) {
	s.latMu.Lock()
	s.latencies.StartupCold.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveStartupColdFailure() {
	s.latMu.Lock()
	s.latencies.StartupCold.ObserveFailure()
	s.latMu.Unlock()
}

func (s *Stats) ObserveStartupWarm(d time.Duration) {
	s.latMu.Lock()
	s.latencies.StartupWarm.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveStartupWarmFailure() {
	s.latMu.Lock()
	s.latencies.StartupWarm.ObserveFailure()
	s.latMu.Unlock()
}

func (s *Stats) ObserveGitClone(d time.Duration) {
	s.latMu.Lock()
	s.latencies.GitClone.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveGitCloneFailure() {
	s.latMu.Lock()
	s.latencies.GitClone.ObserveFailure()
	s.latMu.Unlock()
}

func (s *Stats) ObserveUntar(d time.Duration) {
	s.latMu.Lock()
	s.latencies.Untar.ObserveUs(float64(d.Microseconds()))
	s.latMu.Unlock()
}

func (s *Stats) ObserveUntarFailure() {
	s.latMu.Lock()
	s.latencies.Untar.ObserveFailure()
	s.latMu.Unlock()
}

func (s *Stats) SnapshotAndReset() protocol.MetricSample {
	s.latMu.Lock()
	lat := s.latencies
	s.latencies = protocol.NewLatencySet()
	s.latMu.Unlock()
	return protocol.MetricSample{
		ReadOps:    s.ReadOps.Swap(0),
		WriteOps:   s.WriteOps.Swap(0),
		CreateOps:  s.CreateOps.Swap(0),
		DeleteOps:  s.DeleteOps.Swap(0),
		ReadBytes:  s.ReadBytes.Swap(0),
		WriteBytes: s.WriteBytes.Swap(0),
		Latencies:  lat,
		Timestamp:  time.Now().UTC(),
	}
}

// FileLedger tracks files this client created so delete/bw/read can target them.
type FileLedger struct {
	mu        sync.Mutex
	indices   []int64
	bwIndices []int64 // files created during bandwidth-write (unique paths for read-back)
	next      int64
	delPos    int // persistent cursor across delete ramp steps
}

func NewFileLedger() *FileLedger {
	return &FileLedger{indices: make([]int64, 0, 4096), bwIndices: make([]int64, 0, 4096)}
}

func (l *FileLedger) Add(idx int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.indices = append(l.indices, idx)
	if idx >= l.next {
		l.next = idx + 1
	}
}

// AddBW records a newly created bandwidth file for later unique read-back.
func (l *FileLedger) AddBW(idx int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.indices = append(l.indices, idx)
	l.bwIndices = append(l.bwIndices, idx)
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

func (l *FileLedger) BWSnapshot() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int64, len(l.bwIndices))
	copy(out, l.bwIndices)
	return out
}

func (l *FileLedger) ClearBW() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bwIndices = l.bwIndices[:0]
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
	l.bwIndices = l.bwIndices[:0]
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
	case protocol.PhaseSoftwareCold:
		return w.runSoftwareStartup(ctx, cmd, true)
	case protocol.PhaseSoftwareWarm:
		return w.runSoftwareStartup(ctx, cmd, false)
	case protocol.PhaseGitClone:
		return w.runGitClone(ctx, cmd)
	case protocol.PhaseUntar:
		return w.runUntar(ctx, cmd)
	case protocol.PhaseCreate:
		w.Ledger.ResetDeleteCursor()
		w.Ledger.ClearBW()
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
		t0 := time.Now()
		err := os.WriteFile(path, payload, 0o644)
		opDur := time.Since(t0)
		if err != nil {
			continue
		}
		w.Stats.ObserveCreate(opDur)
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
		t0 := time.Now()
		err := os.Remove(path)
		opDur := time.Since(t0)
		if err != nil {
			continue
		}
		w.Stats.ObserveDelete(opDur)
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
		// Fill the whole buffer — a 4 KiB random head with zero tail is highly
		// compressible and makes gateway/backend traffic look far below client Write() bytes.
		_, _ = rand.Read(buf)
	}

	var (
		readFiles   []int64
		readCursor  int
		transferred float64
		start       = time.Now()
		file        *os.File
		fileOff     int64
		writing     bool
		activeIdx   int64 = -1
		fileOpStart time.Time
		lastRefresh time.Time
	)

	refreshReads := func() {
		readFiles = w.Ledger.BWSnapshot()
		lastRefresh = time.Now()
		if len(readFiles) == 0 {
			return
		}
		if readCursor >= len(readFiles) {
			readCursor = 0
		}
	}

	closeFile := func() {
		if file != nil {
			_ = file.Close()
			file = nil
		}
		fileOff = 0
		writing = false
		activeIdx = -1
		fileOpStart = time.Time{}
	}
	defer closeFile()

	openNextWrite := func() error {
		closeFile()
		idx := w.Ledger.NextIndex()
		path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
		if err := EnsureParent(path); err != nil {
			return err
		}
		t0 := time.Now()
		f, err := openDirectWrite(path)
		if err != nil {
			// Failed O_DIRECT|O_CREAT often leaves an empty file (AFS/tmpfs/VFS);
			// remove it so the buffered O_EXCL open can succeed.
			_ = os.Remove(path)
			f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
		}
		file = f
		writing = true
		activeIdx = idx
		fileOpStart = t0
		return nil
	}

	openNextRead := func() error {
		closeFile()
		if len(readFiles) == 0 || time.Since(lastRefresh) > time.Second {
			refreshReads()
		}
		if len(readFiles) == 0 {
			// No bandwidth files yet — seed one unique incompressible file, then read it.
			idx := w.Ledger.NextIndex()
			path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
			if err := EnsureParent(path); err != nil {
				return err
			}
			_, _ = rand.Read(buf)
			if err := writeFileDirect(path, buf, fileSize); err != nil {
				return err
			}
			w.Ledger.AddBW(idx)
			refreshReads()
		}
		if len(readFiles) == 0 {
			return fmt.Errorf("no bandwidth files to read")
		}
		idx := readFiles[readCursor%len(readFiles)]
		readCursor++
		path := FilePath(cmd.Prefix, cmd.TestName, w.Hostname, idx)
		t0 := time.Now()
		f, err := openDirectRead(path)
		if err != nil {
			f, err = os.Open(path)
			if err != nil {
				return err
			}
		}
		file = f
		writing = false
		activeIdx = idx
		fileOpStart = t0
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

		// Don't start another chunk if the phase window is essentially over.
		remainPhase := time.Until(deadline)
		if remainPhase < 20*time.Millisecond {
			return nil
		}

		if file == nil {
			var err error
			if doWrite {
				err = openNextWrite()
			} else {
				err = openNextRead()
			}
			if err != nil {
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
				if !fileOpStart.IsZero() {
					w.Stats.ObserveWrite(time.Since(fileOpStart))
				}
				w.Stats.WriteOps.Add(1)
				if activeIdx >= 0 {
					w.Ledger.AddBW(activeIdx)
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
					closeFile()
					continue
				}
			}
			// Bound blocking AFS/network Write() so work cannot spill far past the phase.
			_ = file.SetWriteDeadline(time.Now().Add(remainPhase))
			n, err := file.Write(buf[:chunk])
			_ = file.SetWriteDeadline(time.Time{}) // clear
			if n > 0 {
				fileOff += int64(n)
				transferred += float64(n)
				w.Stats.WriteBytes.Add(int64(n))
			}
			if err != nil {
				closeFile()
				if time.Now().After(deadline) || ctx.Err() != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return nil
				}
				continue
			}
			if fileOff >= fileSize {
				if !fileOpStart.IsZero() {
					w.Stats.ObserveWrite(time.Since(fileOpStart))
				}
				w.Stats.WriteOps.Add(1)
				if activeIdx >= 0 {
					w.Ledger.AddBW(activeIdx)
				}
				closeFile()
			}
			continue
		}

		// Read unique bandwidth files (not rewriting the same cached path).
		remain := fileSize - fileOff
		if remain <= 0 {
			if fileOff > 0 {
				if !fileOpStart.IsZero() {
					w.Stats.ObserveRead(time.Since(fileOpStart))
				}
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
					if !fileOpStart.IsZero() {
						w.Stats.ObserveRead(time.Since(fileOpStart))
					}
					w.Stats.ReadOps.Add(1)
				}
				closeFile()
				continue
			}
		}
		_ = file.SetReadDeadline(time.Now().Add(remainPhase))
		n, err := file.Read(buf[:chunk])
		_ = file.SetReadDeadline(time.Time{})
		if n > 0 {
			transferred += float64(n)
			w.Stats.ReadBytes.Add(int64(n))
			fileOff += int64(n)
		}
		if err == io.EOF || fileOff >= fileSize {
			if fileOff > 0 {
				if !fileOpStart.IsZero() {
					w.Stats.ObserveRead(time.Since(fileOpStart))
				}
				w.Stats.ReadOps.Add(1)
			}
			closeFile()
			continue
		}
		if err != nil {
			closeFile()
			if time.Now().After(deadline) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return nil
			}
			continue
		}
	}
}

func writeFileDirect(path string, buf []byte, size int64) error {
	f, err := openDirectWrite(path)
	if err != nil {
		_ = os.Remove(path)
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
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

// Cleanup removes test host files and any software tree under the test name.
func (w *Worker) Cleanup(prefix, testName string) error {
	root := HostRoot(prefix, testName, w.Hostname)
	err := os.RemoveAll(root)
	if err2 := os.RemoveAll(SoftwareDir(prefix, testName)); err == nil {
		err = err2
	}
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
