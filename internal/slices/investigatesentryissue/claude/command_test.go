package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Anthropic:  Credential{Value: "anthropic-key"},
		OpenAIKey:  "openai-key",
		PluginDirs: []string{"/p/codex", "/p/auto-task"},
		Bin:        "/opt/claude/bin/claude",
		Path:       "/usr/bin:/bin",
		ParentHome: "/home/underling",
		DenyRead:   []string{"/srv/underling-data"},
		Readable:   []string{"/p/codex", "/p/auto-task", "/opt/claude/bin"},
	}
}

func testDirs() Dirs {
	return Dirs{
		WorkDir:      "/w",
		Siblings:     []string{"/w/old-run"},
		Mirror:       "/w/run/mirror.git",
		Clone:        "/w/run/repo",
		Home:         "/w/run/home",
		ClaudeConfig: "/w/run/claude-config",
		GitHome:      "/w/run/git-home",
	}
}

// settingsJSON returns the generated settings as JSON.
func settingsJSON(t *testing.T, cfg Config) []byte {
	t.Helper()
	raw, err := json.Marshal(sandboxSettings(cfg, testDirs()))
	if err != nil {
		t.Fatalf("encode settings: %v", err)
	}
	return raw
}

func TestCommandArgv(t *testing.T) {
	cmd, err := Command(testConfig(), testDirs(), "the prompt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	settings := settingsJSON(t, testConfig())
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
	cmd, err := Command(testConfig(), testDirs(), "p")
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

// oauthConfig is testConfig logging in with a subscription token.
func oauthConfig() Config {
	cfg := testConfig()
	cfg.Anthropic = Credential{OAuth: true, Value: "oauth-token"}
	return cfg
}

func TestCommandArgvWithAnOAuthTokenSkipsBareButNotMCPIsolation(t *testing.T) {
	cmd, err := Command(oauthConfig(), testDirs(), "the prompt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{
		"--strict-mcp-config", "-p",
		"--verbose", "--output-format", "stream-json",
		"--no-session-persistence",
		"--setting-sources", "",
		"--settings", string(settingsJSON(t, oauthConfig())),
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
		"--plugin-dir", "/p/codex",
		"--plugin-dir", "/p/auto-task",
	}
	if !slices.Equal(cmd.args, want) {
		t.Fatalf("args = %q, want %q", cmd.args, want)
	}
}

func TestCommandEnvWithAnOAuthTokenHoldsTheTokenInsteadOfAKey(t *testing.T) {
	cmd, err := Command(oauthConfig(), testDirs(), "p")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/w/run/home",
		"CLAUDE_CONFIG_DIR=/w/run/claude-config",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"OPENAI_API_KEY=openai-key",
	}
	if !slices.Equal(cmd.env, want) {
		t.Fatalf("env = %q, want %q", cmd.env, want)
	}
}

func TestSandboxSettingsSandboxTheClone(t *testing.T) {
	raw := settingsJSON(t, testConfig())
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
				"Read(//home/underling/**)",
				"Read(//srv/underling-data/**)",
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
				"denyRead":   []any{"/home/underling", "/srv/underling-data", "/w"},
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
	got := sandboxSettings(cfg, testDirs())
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
	cfg.DenyRead = []string{link, "/srv/underling-data"}
	got := sandboxSettings(cfg, testDirs())

	if want := []string{link, resolved, "/srv/underling-data", "/w"}; !slices.Equal(got.Sandbox.Filesystem.DenyRead, want) {
		t.Errorf("denyRead = %q, want %q", got.Sandbox.Filesystem.DenyRead, want)
	}
	wantRules := []string{"Read(//proc/**)", readDenyRule(link), readDenyRule(resolved), "Read(//srv/underling-data/**)"}
	if len(got.Permissions.Deny) < len(wantRules) || !slices.Equal(got.Permissions.Deny[:len(wantRules)], wantRules) {
		t.Errorf("deny = %q, want it to start with %q", got.Permissions.Deny, wantRules)
	}
}

// bedrockConfig is testConfig in Bedrock mode, without the two keys.
func bedrockConfig(sessionToken string) Config {
	cfg := testConfig()
	cfg.Anthropic, cfg.OpenAIKey = Credential{}, ""
	cfg.Bedrock = &Bedrock{
		Region:         "eu-central-1",
		Model:          "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		SmallFastModel: "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		CodexRegion:    "eu-west-1",
		Credentials:    AWSCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "aws-secret", SessionToken: sessionToken},
	}
	return cfg
}

func TestCommandArgvInBedrockModeKeepsBare(t *testing.T) {
	cfg := bedrockConfig("aws-session")
	cmd, err := Command(cfg, testDirs(), "the prompt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{
		"--bare", "-p",
		"--verbose", "--output-format", "stream-json",
		"--no-session-persistence",
		"--setting-sources", "",
		"--settings", string(settingsJSON(t, cfg)),
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
		"--plugin-dir", "/p/codex",
		"--plugin-dir", "/p/auto-task",
	}
	if !slices.Equal(cmd.args, want) {
		t.Fatalf("args = %q, want %q", cmd.args, want)
	}
}

func TestCommandEnvInBedrockModeHoldsTheAWSCredentialsInsteadOfKeys(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/w/run/home",
		"CLAUDE_CONFIG_DIR=/w/run/claude-config",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"AWS_REGION=eu-central-1",
		"ANTHROPIC_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"ANTHROPIC_SMALL_FAST_MODEL=eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"ANTHROPIC_DEFAULT_FABLE_MODEL=global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		"AWS_ACCESS_KEY_ID=AKIATEST",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
	}
	for name, tt := range map[string]struct {
		sessionToken string
		want         []string
	}{
		"with a session token":    {"aws-session", append(slices.Clone(base), "AWS_SESSION_TOKEN=aws-session")},
		"without a session token": {"", base},
	} {
		t.Run(name, func(t *testing.T) {
			cmd, err := Command(bedrockConfig(tt.sessionToken), testDirs(), "p")
			if err != nil {
				t.Fatalf("Command: %v", err)
			}
			if !slices.Equal(cmd.env, tt.want) {
				t.Fatalf("env = %q, want %q", cmd.env, tt.want)
			}
		})
	}
}

func TestSandboxSettingsInBedrockModeAllowOnlyCodexsBedrockHost(t *testing.T) {
	got := sandboxSettings(bedrockConfig(""), testDirs())
	if want := []string{"bedrock-mantle.eu-west-1.api.aws"}; !slices.Equal(got.Sandbox.Network.AllowedDomains, want) {
		t.Fatalf("allowedDomains = %q, want %q", got.Sandbox.Network.AllowedDomains, want)
	}
	if want := sandboxSettings(testConfig(), testDirs()); !reflect.DeepEqual(got.Permissions, want.Permissions) || !reflect.DeepEqual(got.Sandbox.Filesystem, want.Sandbox.Filesystem) {
		t.Errorf("Bedrock mode changed more than the network: %+v", got)
	}
}
