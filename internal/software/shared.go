package software

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// GitBundleFileName is the shared bundle under <prefix>/<test>/software/.
	GitBundleFileName = "repo.bundle"
)

// GitBundlePath is <prefix>/<testName>/software/repo.bundle.
func GitBundlePath(prefix, testName string) string {
	return filepath.Join(Dir(prefix, testName), GitBundleFileName)
}

// UntarArchiveName picks a stable file name for the shared archive from its URL.
func UntarArchiveName(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
		u = parsed.Path
	}
	base := filepath.Base(u)
	if base == "" || base == "." || base == "/" {
		return "archive" + ArchiveSuffix(rawURL)
	}
	return base
}

// UntarArchivePath is <prefix>/<testName>/software/<archive-name>.
func UntarArchivePath(prefix, testName, rawURL string) string {
	return filepath.Join(Dir(prefix, testName), UntarArchiveName(rawURL))
}

// ArchiveSuffix returns a conventional archive extension from a URL.
func ArchiveSuffix(rawURL string) string {
	u := strings.ToLower(rawURL)
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
		return ".tar.gz"
	}
}

// BundleToPrefixes fetches gitURL once, creates a bundle, and copies it into each
// prefix's software dir as repo.bundle for clients to clone from.
func BundleToPrefixes(ctx context.Context, gitURL string, prefixes []string, testName string) error {
	gitURL = strings.TrimSpace(gitURL)
	if gitURL == "" {
		return fmt.Errorf("git_clone_url is empty")
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("no prefixes configured")
	}

	scratch, err := os.MkdirTemp("", "homebench-git-prep-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	bare := filepath.Join(scratch, "bare.git")
	bundle := filepath.Join(scratch, GitBundleFileName)

	log.Printf("software: git clone --bare %s", gitURL)
	if err := runGit(ctx, scratch, "clone", "--bare", gitURL, bare); err != nil {
		return fmt.Errorf("git clone --bare: %w", err)
	}
	log.Printf("software: git bundle create %s", bundle)
	if err := runGit(ctx, bare, "bundle", "create", bundle, "--all"); err != nil {
		return fmt.Errorf("git bundle create: %w", err)
	}

	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		dir := Dir(prefix, testName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		dest := GitBundlePath(prefix, testName)
		log.Printf("software: installing git bundle → %s", dest)
		if err := copyFile(bundle, dest); err != nil {
			return fmt.Errorf("install bundle into %s: %w", dest, err)
		}
	}
	return nil
}

// DownloadUntarToPrefixes downloads the archive once and copies it into each
// prefix's software dir for clients to tar-extract from.
func DownloadUntarToPrefixes(ctx context.Context, archiveURL string, prefixes []string, testName string) error {
	archiveURL = strings.TrimSpace(archiveURL)
	if archiveURL == "" {
		return fmt.Errorf("untar_url is empty")
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("no prefixes configured")
	}

	tmp, err := os.CreateTemp("", "homebench-untar-*"+ArchiveSuffix(archiveURL))
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	log.Printf("software: downloading untar archive %s", archiveURL)
	if err := DownloadFile(ctx, archiveURL, tmpPath); err != nil {
		return err
	}

	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		dir := Dir(prefix, testName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		dest := UntarArchivePath(prefix, testName, archiveURL)
		log.Printf("software: installing untar archive → %s", dest)
		if err := copyFile(tmpPath, dest); err != nil {
			return fmt.Errorf("install archive into %s: %w", dest, err)
		}
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
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

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
