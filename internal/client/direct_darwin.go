//go:build darwin

package client

import (
	"os"
	"syscall"
)

// F_NOCACHE bypasses the buffer cache on macOS (closest equivalent to O_DIRECT).
const fNoCache = 48

func setNoCache(f *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fNoCache, 1)
	if errno != 0 {
		return errno
	}
	return nil
}

func openDirectWrite(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := setNoCache(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openDirectRead(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := setNoCache(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openDirectReadWrite(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := setNoCache(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
