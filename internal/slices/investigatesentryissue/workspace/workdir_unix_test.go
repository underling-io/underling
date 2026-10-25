//go:build unix

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTheDefaultWorkDirIsPerUser(t *testing.T) {
	want := filepath.Join(os.TempDir(), "heinzel-investigations-"+strconv.Itoa(os.Getuid()))
	if got := DefaultDir(); got != want {
		t.Fatalf("DefaultDir() = %s, want %s", got, want)
	}
}

func TestCheckDirAcceptsAPrivateDirOfOurOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := CheckDir(dir); err != nil {
		t.Fatalf("CheckDir: %v", err)
	}
}

func TestCheckDirRejectsASymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	err := CheckDir(link)

	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v, want an error naming the symlink", err)
	}
}

func TestCheckDirRejectsADirOthersCanUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	err := CheckDir(dir)

	if err == nil || !strings.Contains(err.Error(), "0777") {
		t.Fatalf("got %v, want an error naming the mode", err)
	}
}

func TestCheckDirRejectsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "work")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckDir(file); err == nil {
		t.Fatal("CheckDir accepted a file")
	}
}

func TestNewRejectsAWorkDirOthersCanUse(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workDir, 0o777); err != nil {
		t.Fatal(err)
	}

	_, err := New(context.Background(), workDir, testRemote(t), testBranch, os.Getenv("PATH"), testToken)

	if err == nil || !strings.Contains(err.Error(), "work dir") {
		t.Fatalf("got %v, want a work dir error", err)
	}
	if entries, _ := os.ReadDir(workDir); len(entries) != 0 {
		t.Errorf("work dir holds %v, want nothing", entries)
	}
}
