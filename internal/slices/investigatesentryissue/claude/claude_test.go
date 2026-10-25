package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"heinzel/internal/slices/investigatesentryissue/workspace"
)

func testConfig() Config {
	return Config{
		AnthropicKey: "anthropic-key",
		OpenAIKey:    "openai-key",
		PluginDirs:   []string{"/p/codex", "/p/auto-task"},
		Bin:          "/opt/claude/bin/claude",
		Path:         "/usr/bin:/bin",
		ParentHome:   "/home/heinzel",
		DenyRead:     []string{"/srv/heinzel-data"},
		Readable:     []string{"/p/codex", "/p/auto-task", "/opt/claude/bin"},
	}
}

func testWorkspace() *workspace.Workspace {
	return &workspace.Workspace{
		WorkDir:      "/w",
		Siblings:     []string{"/w/old-run"},
		RunDir:       "/w/run",
		Mirror:       "/w/run/mirror.git",
		Clone:        "/w/run/repo",
		Home:         "/w/run/home",
		ClaudeConfig: "/w/run/claude-config",
		GitHome:      "/w/run/git-home",
		Branch:       "heinzel/sentry-42",
	}
}

func TestCommandArgv(t *testing.T) {
	cmd, err := Command(testConfig(), testWorkspace(), "the prompt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	settings, err := sandboxSettings(testConfig(), testWorkspace())
	if err != nil {
		t.Fatalf("sandboxSettings: %v", err)
	}
	want := []string{
		"--bare", "-p",
		"--verbose", "--output-format", "stream-json",
		"--no-session-persistence",
		"--setting-sources", "",
		"--settings", string(settings),
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
		"--plugin-dir", "/p/codex",
		"--plugin-dir", "/p/auto-task",
	}
	if cmd.bin != "/opt/claude/bin/claude" || cmd.dir != "/w/run/repo" || !slices.Equal(cmd.args, want) {
		t.Fatalf("got %s %q in %s, want /opt/claude/bin/claude %q in /w/run/repo", cmd.bin, cmd.args, cmd.dir, want)
	}
	if cmd.stdin != "the prompt" {
		t.Fatalf("stdin = %q, want the prompt", cmd.stdin)
	}
}

func TestCommandEnvHoldsOnlyWhatTheChildNeeds(t *testing.T) {
	cmd, err := Command(testConfig(), testWorkspace(), "p")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/w/run/home",
		"CLAUDE_CONFIG_DIR=/w/run/claude-config",
		"ANTHROPIC_API_KEY=anthropic-key",
		"OPENAI_API_KEY=openai-key",
	}
	if !slices.Equal(cmd.env, want) {
		t.Fatalf("env = %q, want %q", cmd.env, want)
	}
}

func TestSandboxSettingsSandboxTheClone(t *testing.T) {
	raw, err := sandboxSettings(testConfig(), testWorkspace())
	if err != nil {
		t.Fatalf("sandboxSettings: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"permissions": map[string]any{
			"defaultMode":                  "acceptEdits",
			"disableBypassPermissionsMode": "disable",
			"deny": []any{
				"Read(//proc/**)",
				"Read(//home/heinzel/**)",
				"Read(//srv/heinzel-data/**)",
				"Read(//w/old-run/**)",
				"Read(//w/run/mirror.git/**)",
				"Read(//w/run/git-home/**)",
			},
		},
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"autoAllowBashIfSandboxed": true,
			"allowUnsandboxedCommands": false,
			"filesystem": map[string]any{
				"allowWrite": []any{"/w/run/repo", "/w/run/home"},
				"denyRead":   []any{"/home/heinzel", "/srv/heinzel-data", "/w"},
				"allowRead":  []any{"/w/run/repo", "/w/run/home", "/w/run/claude-config", "/p/codex", "/p/auto-task", "/opt/claude/bin"},
			},
			"network": map[string]any{"allowedDomains": []any{"api.openai.com"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %s, want %v", raw, want)
	}
}

func TestSandboxSettingsWithoutAParentHome(t *testing.T) {
	cfg := testConfig()
	cfg.ParentHome, cfg.DenyRead = "", nil
	raw, err := sandboxSettings(cfg, testWorkspace())
	if err != nil {
		t.Fatalf("sandboxSettings: %v", err)
	}
	var got settings
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := []string{"/w"}; !slices.Equal(got.Sandbox.Filesystem.DenyRead, want) {
		t.Errorf("denyRead = %q, want %q", got.Sandbox.Filesystem.DenyRead, want)
	}
	if slices.ContainsFunc(got.Permissions.Deny, func(r string) bool { return strings.HasPrefix(r, "Read(///") }) {
		t.Errorf("deny holds a rule for an empty path: %q", got.Permissions.Deny)
	}
}

func TestSandboxSettingsDenySymlinkedPathsAndTheirTargets(t *testing.T) {
	target := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "data-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ParentHome = link
	cfg.DenyRead = []string{link, "/srv/heinzel-data"}

	raw, err := sandboxSettings(cfg, testWorkspace())
	if err != nil {
		t.Fatalf("sandboxSettings: %v", err)
	}
	var got settings
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if want := []string{link, resolved, "/srv/heinzel-data", "/w"}; !slices.Equal(got.Sandbox.Filesystem.DenyRead, want) {
		t.Errorf("denyRead = %q, want %q", got.Sandbox.Filesystem.DenyRead, want)
	}
	wantRules := []string{"Read(//proc/**)", readDenyRule(link), readDenyRule(resolved), "Read(//srv/heinzel-data/**)"}
	if len(got.Permissions.Deny) < len(wantRules) || !slices.Equal(got.Permissions.Deny[:len(wantRules)], wantRules) {
		t.Errorf("deny = %q, want it to start with %q", got.Permissions.Deny, wantRules)
	}
}
