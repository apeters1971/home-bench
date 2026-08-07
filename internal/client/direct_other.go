//go:build !linux && !darwin

package client

import "os"

// Fallback platforms: ordinary buffered I/O.
func openDirectWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_SYNC, 0o644)
}

func openDirectRead(path string) (*os.File, error) {
	return os.Open(path)
}

func openDirectReadWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_SYNC, 0o644)
}
