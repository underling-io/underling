package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// WithResolved returns the non-empty paths, each followed by its
// symlink-resolved form when that differs, without duplicates. Denying
// both keeps the child out whichever form it reaches the path by.
func WithResolved(paths []string) []string {
	var out []string
	add := func(p string) {
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		add(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			add(resolved)
		}
	}
	return out
}

// ResolvePath returns path with symlinks resolved, or just cleaned if it
// can't be resolved (e.g. it doesn't exist).
func ResolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// Within reports whether path is root or lies under it.
func Within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// FirstContaining returns the first of roots that path lies within.
func FirstContaining(roots []string, path string) (string, bool) {
	for _, r := range roots {
		if Within(path, r) {
			return r, true
		}
	}
	return "", false
}

// LookPath finds bin as the child would: as given if it holds a path
// separator, otherwise in the dirs of path.
func LookPath(bin, path string) (string, bool) {
	if strings.ContainsAny(bin, "/"+string(filepath.Separator)) {
		return bin, true
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, bin)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && filepath.IsAbs(candidate) {
			return candidate, true
		}
	}
	return "", false
}
