//go:build !linux && !darwin

package client

import "os"

// Fallback platforms: ordinary buffered I/O (no O_SYNC — see AFS / network FS).
func openDirectWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
}

func openDirectRead(path string) (*os.File, error) {
	return os.Open(path)
}

func openDirectReadWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}
