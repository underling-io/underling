//go:build unix

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// DefaultDir is a work dir of this user's own in the shared temp dir.
func DefaultDir() string {
	return filepath.Join(os.TempDir(), defaultDirName+"-"+strconv.Itoa(os.Getuid()))
}

// CheckDir fails unless dir is a real dir (no symlink), owned by this user,
// that neither group nor others can use. In a shared temp dir, another
// user could have made it first, to read or plant run dirs.
func CheckDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check work dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("work dir %s is a symlink; use a real directory", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("work dir %s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("work dir %s is not owned by this user (uid %d)", dir, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("work dir %s has mode %#o; it must give the group and others no access (e.g. 0700)", dir, perm)
	}
	return nil
}
