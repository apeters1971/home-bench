package client

import (
	"fmt"
	"os"
	"path/filepath"
)

// FilePath builds prefix/testname/hostname/shard1/shard2/fileindex
func FilePath(prefix, testName, hostname string, index int64) string {
	shard1 := index / 10000
	shard2 := (index / 100) % 100
	return filepath.Join(
		prefix,
		testName,
		hostname,
		fmt.Sprintf("%04d", shard1),
		fmt.Sprintf("%02d", shard2),
		fmt.Sprintf("%08d", index),
	)
}

func HostRoot(prefix, testName, hostname string) string {
	return filepath.Join(prefix, testName, hostname)
}

func EnsureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
