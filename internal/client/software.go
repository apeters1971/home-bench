package client

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apeters/homebench/internal/protocol"
	"github.com/apeters/homebench/internal/software"
)

const (
	softwareStartupColdTimeout = protocol.SoftwareStartupColdTimeout
	softwareStartupWarmTimeout = protocol.SoftwareStartupWarmTimeout
)

func (w *Worker) softwareDir(cmd protocol.PhaseCommand) string {
	return software.Dir(cmd.Prefix, cmd.TestName)
}

// prepareHostWorkDir clears/creates a per-machine subdirectory under
// <prefix>/<test>/<hostname>/<sub> for client-local ops (git/untar).
func (w *Worker) prepareHostWorkDir(cmd protocol.PhaseCommand, sub string) (string, error) {
	dir := filepath.Join(HostRoot(cmd.Prefix, cmd.TestName, w.Hostname), sub)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

func (w *Worker) runSoftwareStartup(ctx context.Context, cmd protocol.PhaseCommand, cold bool) error {
	dir := w.softwareDir(cmd)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		if cold {
			w.Stats.ObserveStartupColdFailure()
		} else {
			w.Stats.ObserveStartupWarmFailure()
		}
		return fmt.Errorf("software dir missing: %s (controller should unpack first)", dir)
	}
	startup := strings.TrimSpace(cmd.StartupCommand)
	if startup == "" {
		if cold {
			w.Stats.ObserveStartupColdFailure()
		} else {
			w.Stats.ObserveStartupWarmFailure()
		}
		return fmt.Errorf("startup_command is empty")
	}

	timeout := time.Duration(cmd.Duration * float64(time.Second))
	if timeout <= 0 {
		if cold {
			timeout = softwareStartupColdTimeout
		} else {
			timeout = softwareStartupWarmTimeout
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	t0 := time.Now()
	err := runShellCommand(runCtx, dir, startup)
	elapsed := time.Since(t0)
	if err != nil {
		if cold {
			w.Stats.ObserveStartupColdFailure()
		} else {
			w.Stats.ObserveStartupWarmFailure()
		}
		return fmt.Errorf("startup (cold=%v) after %s: %w", cold, elapsed, err)
	}
	if cold {
		w.Stats.ObserveStartupCold(elapsed)
	} else {
		w.Stats.ObserveStartupWarm(elapsed)
	}
	// Shared software tree is removed by the controller after warm completes,
	// so clients can report idle immediately (RemoveAll would block the phase).
	return nil
}

func (w *Worker) runGitClone(ctx context.Context, cmd protocol.PhaseCommand) error {
	bundle := software.GitBundlePath(cmd.Prefix, cmd.TestName)
	if st, err := os.Stat(bundle); err != nil || st.IsDir() {
		w.Stats.ObserveGitCloneFailure()
		return fmt.Errorf("shared git bundle missing: %s (controller should prepare first)", bundle)
	}

	dir, err := w.prepareHostWorkDir(cmd, "git")
	if err != nil {
		w.Stats.ObserveGitCloneFailure()
		return err
	}

	t0 := time.Now()
	err = runShellCommand(ctx, dir, "git clone "+shellQuote(bundle)+" repo")
	elapsed := time.Since(t0)
	var files int64
	if err == nil {
		files = countRegularFiles(dir)
	}
	// Cleanup after measurement so histogram excludes delete time.
	_ = os.RemoveAll(dir)
	if err != nil {
		w.Stats.ObserveGitCloneFailure()
		return fmt.Errorf("git clone from shared bundle after %s: %w", elapsed, err)
	}
	w.Stats.GitFiles.Add(files)
	w.Stats.ObserveGitClone(elapsed)
	return nil
}

func (w *Worker) runUntar(ctx context.Context, cmd protocol.PhaseCommand) error {
	url := strings.TrimSpace(cmd.UntarURL)
	if url == "" {
		w.Stats.ObserveUntarFailure()
		return fmt.Errorf("untar_url is empty")
	}
	archive := software.UntarArchivePath(cmd.Prefix, cmd.TestName, url)
	if st, err := os.Stat(archive); err != nil || st.IsDir() {
		w.Stats.ObserveUntarFailure()
		return fmt.Errorf("shared untar archive missing: %s (controller should prepare first)", archive)
	}

	dir, err := w.prepareHostWorkDir(cmd, "untar")
	if err != nil {
		w.Stats.ObserveUntarFailure()
		return err
	}

	t0 := time.Now()
	err = runShellCommand(ctx, dir, "tar xvf "+shellQuote(archive))
	elapsed := time.Since(t0)
	var files int64
	if err == nil {
		files = countRegularFiles(dir)
	}
	// Cleanup after measurement so histogram excludes delete time.
	_ = os.RemoveAll(dir)
	if err != nil {
		w.Stats.ObserveUntarFailure()
		return fmt.Errorf("tar xvf after %s: %w", elapsed, err)
	}
	w.Stats.UntarFiles.Add(files)
	w.Stats.ObserveUntar(elapsed)
	return nil
}

// countRegularFiles returns how many non-directory entries exist under root.
func countRegularFiles(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func runShellCommand(ctx context.Context, dir, command string) error {
	c := exec.CommandContext(ctx, "bash", "-lc", command)
	c.Dir = dir
	c.Env = os.Environ()
	out, err := c.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
