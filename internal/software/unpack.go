package software

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Dir is <prefix>/<testName>/software — shared package tree for all clients on a prefix.
func Dir(prefix, testName string) string {
	return filepath.Join(prefix, testName, "software")
}

// UnpackToPrefixes downloads packageURL once and extracts into each prefix's software dir.
func UnpackToPrefixes(ctx context.Context, packageURL string, prefixes []string, testName string) error {
	url := strings.TrimSpace(packageURL)
	if url == "" {
		return fmt.Errorf("package_url is empty")
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("no prefixes configured")
	}

	tmp, err := os.CreateTemp("", "homebench-package-*.tar.gz")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	log.Printf("software: downloading %s", url)
	if err := DownloadFile(ctx, url, tmpPath); err != nil {
		return err
	}

	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		dir := Dir(prefix, testName)
		log.Printf("software: extracting into %s", dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clear %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		if err := ExtractTarGz(ctx, tmpPath, dir); err != nil {
			return fmt.Errorf("extract into %s: %w", dir, err)
		}
	}
	return nil
}

// DownloadFile fetches url into dest.
func DownloadFile(ctx context.Context, url, dest string) error {
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

// ExtractTarGz extracts a gzip-compressed tar archive into dest.
func ExtractTarGz(ctx context.Context, archive, dest string) error {
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
