//go:build linux

package client

import (
	"os"
	"syscall"
)

func openDirectWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_DIRECT, 0o644)
}

func openDirectRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
}

func openDirectReadWrite(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_DIRECT, 0o644)
}
