//go:build !unix

package investigatesentryissue

import (
	"os"
	"path/filepath"
)

// defaultWorkDir is a work dir in the OS temp dir, which is per-user on
// Windows.
func defaultWorkDir() string {
	return filepath.Join(os.TempDir(), defaultWorkDirName)
}

// checkWorkDir checks nothing: ownership and mode bits are Unix concepts.
func checkWorkDir(string) error { return nil }
