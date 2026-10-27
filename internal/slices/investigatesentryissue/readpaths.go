package investigatesentryissue

import (
	"fmt"
	"path/filepath"
	"slices"

	"heinzel/internal/slices/investigatesentryissue/claude"
)

// resolveReadPaths checks that what the child must read stays readable
// despite its read denials, and sets cfg.readable. cfg.workDir must be set.
//
// Claude's file tools are kept out of heinzel's HOME and the deny-read
// paths by Read deny rules, which nothing can carve out, so the work dir
// and the plugin dirs must lie outside them. Sandboxed commands are also
// denied the work dir. A narrower allowRead re-opens a path inside a denied
// one, never a denied path inside it, so no denied path may lie inside a
// plugin dir or the claude binary's dir.
func resolveReadPaths(cfg *config) error {
	var fileDenied []string
	for _, p := range append([]string{cfg.parentHome}, cfg.denyRead...) {
		if p != "" {
			fileDenied = append(fileDenied, claude.ResolvePath(p))
		}
	}
	workDir := claude.ResolvePath(cfg.workDir)
	sandboxDenied := append(slices.Clone(fileDenied), workDir)

	if d, ok := claude.FirstContaining(fileDenied, workDir); ok {
		return fmt.Errorf("%s: %s lies under %s, which the child may not read (HOME or %s)", workDirEnvVar, cfg.workDir, d, denyReadEnvVar)
	}
	readable := []string{}
	for _, dir := range cfg.pluginDirs {
		p := claude.ResolvePath(dir)
		for _, d := range sandboxDenied {
			if claude.Within(p, d) || claude.Within(d, p) {
				return fmt.Errorf("%s: plugin dir %s overlaps %s, which the child may not read (HOME, %s or %s); move the plugins elsewhere", pluginDirsEnvVar, dir, d, denyReadEnvVar, workDirEnvVar)
			}
		}
		readable = append(readable, p)
	}
	if bin, ok := claude.LookPath(cfg.claudeBin, cfg.path); ok {
		binDir := filepath.Dir(claude.ResolvePath(bin))
		for _, d := range sandboxDenied {
			if claude.Within(d, binDir) {
				return fmt.Errorf("%s: %s lies in %s, which holds %s, a path the child may not read", claudeBinEnvVar, cfg.claudeBin, binDir, d)
			}
		}
		readable = append(readable, binDir)
	}
	cfg.readable = readable
	return nil
}

// deniedPathDir is a PATH entry under a path the child may not read.
type deniedPathDir struct {
	dir, denied string
}

// deniedPathDirs returns the PATH entries that lie, as given or with
// symlinks resolved, under HOME, a deny-read path or the work dir, and that
// no readable path re-opens: sandboxed commands can't run tools from them.
// cfg.workDir and cfg.readable must be set.
func deniedPathDirs(cfg config) []deniedPathDir {
	denied := claude.WithResolved(append([]string{cfg.parentHome, cfg.workDir}, cfg.denyRead...))
	reopened := func(p string) bool { _, ok := claude.FirstContaining(cfg.readable, p); return ok }
	var found []deniedPathDir
	for _, dir := range filepath.SplitList(cfg.path) {
		if dir == "" {
			continue
		}
		forms := claude.WithResolved([]string{filepath.Clean(dir)})
		if slices.ContainsFunc(forms, reopened) {
			continue
		}
		for _, f := range forms {
			if d, ok := claude.FirstContaining(denied, f); ok {
				found = append(found, deniedPathDir{dir: dir, denied: d})
				break
			}
		}
	}
	return found
}
