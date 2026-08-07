//go:build linux

package client

import (
	"os"
	"syscall"
)

func openDirectWrite(path string) (*os.File, error) {
	// O_EXCL: always a new inode/path — avoids gateway cache hits on rewrites.
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL|os.O_SYNC|syscall.O_DIRECT, 0o644)
}

func openDirectRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
}

func openDirectReadWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_SYNC|syscall.O_DIRECT, 0o644)
}
