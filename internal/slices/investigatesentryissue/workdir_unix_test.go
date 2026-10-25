//go:build unix

package investigatesentryissue

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTheDefaultWorkDirIsPerUser(t *testing.T) {
	want := filepath.Join(os.TempDir(), "heinzel-investigations-"+strconv.Itoa(os.Getuid()))
	if got := defaultWorkDir(); got != want {
		t.Fatalf("defaultWorkDir() = %s, want %s", got, want)
	}
}

func TestCheckWorkDirAcceptsAPrivateDirOfOurOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := checkWorkDir(dir); err != nil {
		t.Fatalf("checkWorkDir: %v", err)
	}
}

func TestCheckWorkDirRejectsASymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	err := checkWorkDir(link)

	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v, want an error naming the symlink", err)
	}
}

func TestCheckWorkDirRejectsADirOthersCanUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	err := checkWorkDir(dir)

	if err == nil || !strings.Contains(err.Error(), "0777") {
		t.Fatalf("got %v, want an error naming the mode", err)
	}
}

func TestCheckWorkDirRejectsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "work")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := checkWorkDir(file); err == nil {
		t.Fatal("checkWorkDir accepted a file")
	}
}
