//go:build !unix

package workspace

import (
	"os"
	"path/filepath"
)

// DefaultDir is a work dir in the OS temp dir, which is per-user on Windows.
func DefaultDir() string {
	return filepath.Join(os.TempDir(), defaultDirName)
}

// CheckDir checks nothing: ownership and mode bits are Unix concepts.
func CheckDir(string) error { return nil }
