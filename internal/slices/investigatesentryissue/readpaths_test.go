package investigatesentryissue

import (
	"reflect"
	"strings"
	"testing"
)

// readPathsConfig is a config whose paths don't exist, so resolving them
// only cleans them.
func readPathsConfig() config {
	return config{
		parentHome: "/home/underling",
		denyRead:   []string{"/srv/data"},
		workDir:    "/var/underling",
		pluginDirs: []string{"/opt/plugins/codex", "/opt/plugins/auto-task/"},
		claudeBin:  "/home/underling/.local/bin/claude",
	}
}

func TestResolveReadPathsKeepsPluginsAndTheClaudeBinaryReadable(t *testing.T) {
	cfg := readPathsConfig()

	if err := resolveReadPaths(&cfg); err != nil {
		t.Fatalf("resolveReadPaths: %v", err)
	}

	want := []string{"/opt/plugins/codex", "/opt/plugins/auto-task", "/home/underling/.local/bin"}
	if !reflect.DeepEqual(cfg.readable, want) {
		t.Fatalf("readable = %q, want %q", cfg.readable, want)
	}
}

func TestResolveReadPathsRejectsPathsTheSandboxCannotReopen(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*config)
		variable string
	}{
		{"plugin dir under HOME", func(c *config) { c.pluginDirs[0] = "/home/underling/.claude/plugins/codex" }, "CLAUDE_PLUGIN_DIRS"},
		{"plugin dir under a denied path", func(c *config) { c.pluginDirs[1] = "/srv/data/plugins/at" }, "CLAUDE_PLUGIN_DIRS"},
		{"plugin dir holding a denied path", func(c *config) { c.pluginDirs[1] = "/srv" }, "CLAUDE_PLUGIN_DIRS"},
		{"plugin dir under the work dir", func(c *config) { c.pluginDirs[0] = "/var/underling/plugins" }, "CLAUDE_PLUGIN_DIRS"},
		{"work dir under HOME", func(c *config) { c.workDir = "/home/underling/work" }, "INVESTIGATION_WORK_DIR"},
		{"work dir under a denied path", func(c *config) { c.workDir = "/srv/data/work" }, "INVESTIGATION_WORK_DIR"},
		{"claude binary directly in HOME", func(c *config) { c.claudeBin = "/home/underling/claude" }, "CLAUDE_BIN"},
		{"claude binary dir holding a denied path", func(c *config) { c.claudeBin = "/srv/claude" }, "CLAUDE_BIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := readPathsConfig()
			tt.change(&cfg)
			err := resolveReadPaths(&cfg)
			if err == nil || !strings.Contains(err.Error(), tt.variable) {
				t.Fatalf("got error %v, want one naming %s", err, tt.variable)
			}
		})
	}
}
