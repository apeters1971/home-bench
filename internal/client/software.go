package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apeters/homebench/internal/protocol"
	"github.com/apeters/homebench/internal/software"
)

const softwareStartupTimeout = 60 * time.Second

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
		return fmt.Errorf("software dir missing: %s (controller should unpack first)", dir)
	}
	startup := strings.TrimSpace(cmd.StartupCommand)
	if startup == "" {
		return fmt.Errorf("startup_command is empty")
	}

	runCtx, cancel := context.WithTimeout(ctx, softwareStartupTimeout)
	defer cancel()

	t0 := time.Now()
	err := runShellCommand(runCtx, dir, startup)
	elapsed := time.Since(t0)
	if cold {
		w.Stats.ObserveStartupCold(elapsed)
	} else {
		w.Stats.ObserveStartupWarm(elapsed)
	}
	if err != nil {
		return fmt.Errorf("startup (cold=%v) after %s: %w", cold, elapsed, err)
	}
	// After warm measurement, free the shared tree before later phases.
	if !cold {
		_ = os.RemoveAll(dir)
	}
	return nil
}

func (w *Worker) runGitClone(ctx context.Context, cmd protocol.PhaseCommand) error {
	dir, err := w.prepareHostWorkDir(cmd, "git")
	if err != nil {
		return err
	}
	url := strings.TrimSpace(cmd.GitCloneURL)
	if url == "" {
		return fmt.Errorf("git_clone_url is empty")
	}

	t0 := time.Now()
	err = runShellCommand(ctx, dir, "git clone "+shellQuote(url))
	elapsed := time.Since(t0)
	w.Stats.ObserveGitClone(elapsed)
	// Cleanup after measurement so histogram excludes delete time.
	_ = os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("git clone after %s: %w", elapsed, err)
	}
	return nil
}

func (w *Worker) runUntar(ctx context.Context, cmd protocol.PhaseCommand) error {
	dir, err := w.prepareHostWorkDir(cmd, "untar")
	if err != nil {
		return err
	}
	url := strings.TrimSpace(cmd.UntarURL)
	if url == "" {
		return fmt.Errorf("untar_url is empty")
	}

	archivePath := filepath.Join(dir, ".archive"+archiveSuffix(url))
	if err := software.DownloadFile(ctx, url, archivePath); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}

	t0 := time.Now()
	err = runShellCommand(ctx, dir, "tar xvf "+shellQuote(filepath.Base(archivePath)))
	elapsed := time.Since(t0)
	w.Stats.ObserveUntar(elapsed)
	// Cleanup after measurement so histogram excludes delete time.
	_ = os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("tar xvf after %s: %w", elapsed, err)
	}
	return nil
}

func archiveSuffix(url string) string {
	u := strings.ToLower(url)
	switch {
	case strings.HasSuffix(u, ".tar.gz"), strings.HasSuffix(u, ".tgz"):
		return ".tar.gz"
	case strings.HasSuffix(u, ".tar.bz2"), strings.HasSuffix(u, ".tbz2"):
		return ".tar.bz2"
	case strings.HasSuffix(u, ".tar.xz"), strings.HasSuffix(u, ".txz"):
		return ".tar.xz"
	case strings.HasSuffix(u, ".tar"):
		return ".tar"
	default:
		return ".tar"
	}
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
