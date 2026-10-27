package investigatesentryissue

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"heinzel/internal/slices/investigatesentryissue/claude"
)

// validEnv holds every required variable with a valid value.
func validEnv() map[string]string {
	return map[string]string{
		"SENTRY_GITHUB_REPOS": "1=acme/web",
		"GITHUB_TOKEN":        "gh-token",
		"ANTHROPIC_API_KEY":   "anthropic-key",
		"OPENAI_API_KEY":      "openai-key",
		"CLAUDE_PLUGIN_DIRS":  "/plugins/codex",
		"PATH":                "/usr/bin:/bin",
	}
}

func with(overrides map[string]string) func(string) string {
	env := validEnv()
	maps.Copy(env, overrides)
	return func(key string) string { return env[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	got, err := loadConfig(with(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := config{
		repos:       map[int64]repo{1: {owner: "acme", name: "web"}},
		githubToken: "gh-token",
		anthropic:   claude.Credential{Value: "anthropic-key"},
		openAIKey:   "openai-key",
		pluginDirs:  []string{"/plugins/codex"},
		timeout:     30 * time.Minute,
		maxAge:      time.Hour,
		claudeBin:   "claude",
		workDir:     "",
		path:        "/usr/bin:/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadConfigReadsEveryVariable(t *testing.T) {
	got, err := loadConfig(with(map[string]string{
		"SENTRY_GITHUB_REPOS":     " 1=acme/web , 42=Other-Org/api.v2_x ",
		"CLAUDE_PLUGIN_DIRS":      "/p/codex, /p/auto-task",
		"INVESTIGATION_TIMEOUT":   "90s",
		"INVESTIGATION_MAX_AGE":   "15m",
		"CLAUDE_BIN":              "/opt/claude",
		"INVESTIGATION_WORK_DIR":  "/var/heinzel",
		"INVESTIGATION_DENY_READ": " /srv/data , /etc/heinzel/ ",
		"HOME":                    "/home/heinzel/",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	wantRepos := map[int64]repo{1: {owner: "acme", name: "web"}, 42: {owner: "Other-Org", name: "api.v2_x"}}
	if !reflect.DeepEqual(got.repos, wantRepos) {
		t.Errorf("repos = %v, want %v", got.repos, wantRepos)
	}
	if want := []string{"/p/codex", "/p/auto-task"}; !reflect.DeepEqual(got.pluginDirs, want) {
		t.Errorf("pluginDirs = %v, want %v", got.pluginDirs, want)
	}
	if want := []string{"/srv/data", "/etc/heinzel"}; !reflect.DeepEqual(got.denyRead, want) || got.parentHome != "/home/heinzel" {
		t.Errorf("denyRead = %q and parentHome = %q, want %q and /home/heinzel", got.denyRead, got.parentHome, want)
	}
	if got.timeout != 90*time.Second || got.maxAge != 15*time.Minute || got.claudeBin != "/opt/claude" || got.workDir != "/var/heinzel" {
		t.Errorf("got %+v", got)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		variable string
		value    string
	}{
		{"SENTRY_GITHUB_REPOS", ""},
		{"SENTRY_GITHUB_REPOS", "1"},
		{"SENTRY_GITHUB_REPOS", "x=acme/web"},
		{"SENTRY_GITHUB_REPOS", "1.5=acme/web"},
		{"SENTRY_GITHUB_REPOS", "1=acme"},
		{"SENTRY_GITHUB_REPOS", "1=acme/web/extra"},
		{"SENTRY_GITHUB_REPOS", "1=/web"},
		{"SENTRY_GITHUB_REPOS", "1=acme/"},
		{"SENTRY_GITHUB_REPOS", "1=acme/we b"},
		{"SENTRY_GITHUB_REPOS", "1=../web"},
		{"SENTRY_GITHUB_REPOS", "1=acme/web,"},
		{"SENTRY_GITHUB_REPOS", "1=acme/web,1=acme/api"},
		{"GITHUB_TOKEN", ""},
		{"OPENAI_API_KEY", ""},
		{"CLAUDE_PLUGIN_DIRS", ""},
		{"CLAUDE_PLUGIN_DIRS", "/p/codex,,/p/at"},
		{"INVESTIGATION_TIMEOUT", "30"},
		{"INVESTIGATION_TIMEOUT", "0s"},
		{"INVESTIGATION_TIMEOUT", "-1m"},
		{"INVESTIGATION_MAX_AGE", "soon"},
		{"INVESTIGATION_MAX_AGE", "0"},
		{"INVESTIGATION_DENY_READ", "relative/dir"},
		{"INVESTIGATION_DENY_READ", "/a,,/b"},
	}
	for _, tt := range tests {
		t.Run(tt.variable+"="+tt.value, func(t *testing.T) {
			_, err := loadConfig(with(map[string]string{tt.variable: tt.value}))
			if err == nil || !strings.Contains(err.Error(), tt.variable) {
				t.Fatalf("got error %v, want one naming %s", err, tt.variable)
			}
		})
	}
}

func TestLoadConfigTakesAnOAuthTokenInsteadOfAnAPIKey(t *testing.T) {
	got, err := loadConfig(with(map[string]string{"ANTHROPIC_API_KEY": "", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-token"}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := (claude.Credential{OAuth: true, Value: "oauth-token"}); got.anthropic != want {
		t.Fatalf("anthropic = %+v, want %+v", got.anthropic, want)
	}
}

func TestLoadConfigWantsExactlyOneAnthropicCredential(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"neither": {"ANTHROPIC_API_KEY": ""},
		"both":    {"CLAUDE_CODE_OAUTH_TOKEN": "oauth-token"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(with(env))
			if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Fatalf("got error %v, want one naming both variables", err)
			}
		})
	}
}
