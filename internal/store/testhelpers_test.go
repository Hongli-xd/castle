package store

import (
	"os"
	"path/filepath"
	"testing"
)

// test helpers used by the crash-semantics tests; they intentionally reach
// into the on-disk format to simulate damage real crashes produce.

func walPathIn(dir string) string { return filepath.Join(dir, "wal.log") }

func openAppend(t *testing.T, dir string) (*os.File, error) {
	t.Helper()
	return os.OpenFile(walPathIn(dir), os.O_WRONLY|os.O_APPEND, 0o600)
}

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func osWriteFile(path string, blob []byte) error {
	return os.WriteFile(path, blob, 0o600)
}

func osStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
