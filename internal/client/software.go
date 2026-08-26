package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/apeters/homebench/internal/protocol"
)

const softwareStartupTimeout = 60 * time.Second

func (w *Worker) runSoftwareUnpack(ctx context.Context, cmd protocol.PhaseCommand) error {
	dir := SoftwareDir(cmd.Prefix)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear software dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir software: %w", err)
	}
	url := strings.TrimSpace(cmd.PackageURL)
	if url == "" {
		return fmt.Errorf("package_url is empty")
	}

	archivePath := filepath.Join(dir, ".package.tar.gz")
	if err := downloadFile(ctx, url, archivePath); err != nil {
		return err
	}
	defer os.Remove(archivePath)

	return extractTarGz(ctx, archivePath, dir)
}

func (w *Worker) runSoftwareStartup(ctx context.Context, cmd protocol.PhaseCommand, cold bool) error {
	dir := SoftwareDir(cmd.Prefix)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("software dir missing: %s", dir)
	}
	startup := strings.TrimSpace(cmd.StartupCommand)
	if startup == "" {
		return fmt.Errorf("startup_command is empty")
	}

	runCtx, cancel := context.WithTimeout(ctx, softwareStartupTimeout)
	defer cancel()

	t0 := time.Now()
	err := runStartupCommand(runCtx, dir, startup)
	elapsed := time.Since(t0)
	if cold {
		w.Stats.ObserveStartupCold(elapsed)
	} else {
		w.Stats.ObserveStartupWarm(elapsed)
	}
	if err != nil {
		return fmt.Errorf("startup (cold=%v) after %s: %w", cold, elapsed, err)
	}
	// After warm measurement, free the unpacked tree before IO phases.
	if !cold {
		_ = os.RemoveAll(dir)
	}
	return nil
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("download write: %w", err)
	}
	return f.Close()
}

func extractTarGz(ctx context.Context, archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if err := extractTarEntry(dest, hdr, tr); err != nil {
			return err
		}
	}
}

func extractTarEntry(dest string, hdr *tar.Header, r io.Reader) error {
	name := filepath.Clean(hdr.Name)
	if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
		return nil
	}
	target := filepath.Join(dest, name)
	rel, err := filepath.Rel(filepath.Clean(dest), target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("tar entry escapes dest: %s", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode)
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, r)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	default:
		return nil
	}
}

func runStartupCommand(ctx context.Context, dir, command string) error {
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
