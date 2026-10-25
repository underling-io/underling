//go:build unix

package investigatesentryissue

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// defaultWorkDir is a work dir of this user's own in the shared OS temp dir.
func defaultWorkDir() string {
	return filepath.Join(os.TempDir(), defaultWorkDirName+"-"+strconv.Itoa(os.Getuid()))
}

// checkWorkDir fails unless dir is a real directory (not a symlink), owned
// by this user, that no group or other user can use. In a shared temp dir,
// another user could have created it first to read or plant run dirs.
func checkWorkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check work dir: %w", err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("work dir %s is a symlink; use a real directory", dir)
	case !info.IsDir():
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
