package investigatesentryissue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"heinzel/internal/events"
)

func testConfig() config {
	return config{
		anthropicKey: "anthropic-key",
		openAIKey:    "openai-key",
		githubToken:  "gh-token",
		pluginDirs:   []string{"/p/codex", "/p/auto-task"},
		claudeBin:    "/opt/claude/bin/claude",
		path:         "/usr/bin:/bin",
		parentHome:   "/home/heinzel",
		denyRead:     []string{"/srv/heinzel-data"},
		readable:     []string{"/p/codex", "/p/auto-task", "/opt/claude/bin"},
	}
}

func testWorkspace() workspace {
	return workspace{
		workDir:      "/w",
		siblings:     []string{"/w/old-run"},
		runDir:       "/w/run",
		mirror:       "/w/run/mirror.git",
		clone:        "/w/run/repo",
		home:         "/w/run/home",
		claudeConfig: "/w/run/claude-config",
		gitHome:      "/w/run/git-home",
		branch:       "heinzel/sentry-42",
	}
}

func TestClaudeCommandArgv(t *testing.T) {
	cmd, err := claudeCommand(testConfig(), testWorkspace(), "the prompt")
	if err != nil {
		t.Fatalf("claudeCommand: %v", err)
	}
	settings, err := claudeSettings(testConfig(), testWorkspace())
	if err != nil {
		t.Fatalf("claudeSettings: %v", err)
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

func TestPromptCapsTheTitle(t *testing.T) {
	long := strings.Repeat("é", maxPromptTitleRunes+100)

	got := investigationPrompt(events.SentryIssueAlertReceived{IssueID: "42", Title: long}, "b")

	if !strings.Contains(got, strings.Repeat("é", maxPromptTitleRunes)) || strings.Contains(got, strings.Repeat("é", maxPromptTitleRunes+1)) {
		t.Fatalf("prompt does not cap the title at %d runes", maxPromptTitleRunes)
	}
}

func TestClaudeCommandEnvHoldsOnlyWhatTheChildNeeds(t *testing.T) {
	cmd, err := claudeCommand(testConfig(), testWorkspace(), "p")
	if err != nil {
		t.Fatalf("claudeCommand: %v", err)
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

func TestClaudeSettingsSandboxTheClone(t *testing.T) {
	raw, err := claudeSettings(testConfig(), testWorkspace())
	if err != nil {
		t.Fatalf("claudeSettings: %v", err)
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

func TestClaudeSettingsWithoutAParentHome(t *testing.T) {
	cfg := testConfig()
	cfg.parentHome, cfg.denyRead = "", nil
	raw, err := claudeSettings(cfg, testWorkspace())
	if err != nil {
		t.Fatalf("claudeSettings: %v", err)
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

func TestClaudeSettingsDenySymlinkedPathsAndTheirTargets(t *testing.T) {
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
	cfg.parentHome = link
	cfg.denyRead = []string{link, "/srv/heinzel-data"}

	raw, err := claudeSettings(cfg, testWorkspace())
	if err != nil {
		t.Fatalf("claudeSettings: %v", err)
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

func TestPromptStartsAutoTaskAndCarriesIssueDetailsAsData(t *testing.T) {
	alert := events.SentryIssueAlertReceived{
		IssueID:        "42",
		Project:        1,
		Title:          "TypeError: x is undefined",
		Level:          "error",
		WebURL:         "https://sentry.io/i/42",
		Culprit:        "app.handler",
		ExceptionType:  "TypeError",
		ExceptionValue: "x is undefined",
		Frames:         []events.SentryStackFrame{{File: "app.js", Function: "handler", Line: 7}},
	}

	got := investigationPrompt(alert, "heinzel/sentry-42")

	if !strings.HasPrefix(got, "/at:auto-task ") {
		t.Errorf("prompt does not start with /at:auto-task: %q", got)
	}
	for _, want := range []string{
		"heinzel/sentry-42",
		"Commit",
		"Do not push",
		"/at:ship-task",
		"summary",
		`"issueId": "42"`,
		`"culprit": "app.handler"`,
		`"exceptionType": "TypeError"`,
		`"exceptionValue": "x is undefined"`,
		`"file": "app.js"`,
		`"webUrl": "https://sentry.io/i/42"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestPromptOverridesAutoTasksWorktreeBranchAndShipSteps(t *testing.T) {
	got := investigationPrompt(events.SentryIssueAlertReceived{IssueID: "42"}, "heinzel/sentry-42")

	for _, want := range []string{
		"Run every /at:auto-task step in place, in the current directory",
		"Do not create a git worktree",
		"do not create, switch or rename branches",
		"commit only on branch heinzel/sentry-42",
		"Skip auto-task's ship and hand-off steps",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
}

func TestFinalResult(t *testing.T) {
	tests := []struct {
		name        string
		stream      string
		wantSummary string
		wantOK      bool
	}{
		{"success", `{"type":"system","subtype":"init"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"Fixed it."}` + "\n", "Fixed it.", true},
		{"no trailing newline", `{"type":"result","is_error":false,"result":"ok"}`, "ok", true},
		{"error result", `{"type":"result","is_error":true,"result":"boom"}` + "\n", "boom", false},
		{"result not last", `{"type":"result","is_error":false,"result":"ok"}` + "\n" + `{"type":"assistant"}` + "\n", "", false},
		{"no events", "", "", false},
		{"noise is skipped", `{"type":"result","is_error":false,"result":"ok"}` + "\nnot json\n\n", "ok", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := readStream(strings.NewReader(tt.stream))
			if err != nil {
				t.Fatalf("readStream: %v", err)
			}
			if out.summary != tt.wantSummary || out.ok != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", out.summary, out.ok, tt.wantSummary, tt.wantOK)
			}
		})
	}
}

func TestFinalResultReadsLongLines(t *testing.T) {
	long := `{"type":"user","content":"` + strings.Repeat("x", 5<<20) + `"}` + "\n"
	out, err := readStream(strings.NewReader(long + `{"type":"result","is_error":false,"result":"ok"}` + "\n"))
	if err != nil || !out.ok || out.summary != "ok" {
		t.Fatalf("got (%q, %v, %v), want (ok, true, nil)", out.summary, out.ok, err)
	}
}

func TestReadStreamKeepsTheInitEvent(t *testing.T) {
	stream := `{"type":"system","subtype":"api_retry"}` + "\n" +
		`{"type":"system","subtype":"init","cwd":"/w/run/repo","permissionMode":"acceptEdits","plugins":[{"name":"codex","path":"/p/codex"}],"plugin_errors":[{"plugin":"at","type":"x","message":"m"}]}` + "\n" +
		`{"type":"result","is_error":false,"result":"ok"}` + "\n"

	out, err := readStream(strings.NewReader(stream))

	if err != nil || out.init == nil {
		t.Fatalf("got init %v, err %v", out.init, err)
	}
	if out.init.PermissionMode != "acceptEdits" || len(out.init.Plugins) != 1 || out.init.Plugins[0].Path != "/p/codex" || len(out.init.PluginErrors) != 1 {
		t.Fatalf("got init %+v", *out.init)
	}
}

func TestCheckInit(t *testing.T) {
	plugins := []string{"/p/codex", "/p/at"}
	good := func() *initEvent {
		return &initEvent{
			PermissionMode: "acceptEdits",
			Plugins:        []initPlugin{{Name: "codex", Path: "/p/codex"}, {Name: "at", Path: "/p/at/plugins/at"}},
		}
	}
	tests := []struct {
		name   string
		init   func() *initEvent
		reason string // empty: no error
	}{
		{"as configured", good, ""},
		{"missing", func() *initEvent { return nil }, "no system/init event"},
		{"other permission mode", func() *initEvent { e := good(); e.PermissionMode = "default"; return e }, `permission mode "default"`},
		{"plugin missing", func() *initEvent { e := good(); e.Plugins = e.Plugins[:1]; return e }, "no plugin loaded from /p/at"},
		{"plugin errors", func() *initEvent {
			e := good()
			e.PluginErrors = []json.RawMessage{json.RawMessage(`{"plugin":"at","type":"hooks","message":"bad hooks file"}`)}
			return e
		}, "bad hooks file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkInit(tt.init(), plugins)
			if tt.reason == "" {
				if err != nil {
					t.Fatalf("checkInit: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("got %v, want an error containing %q", err, tt.reason)
			}
		})
	}
}
