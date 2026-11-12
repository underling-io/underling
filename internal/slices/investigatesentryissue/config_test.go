package investigatesentryissue

import (
	"maps"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"underling/internal/slices/investigatesentryissue/claude"
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
		maxBundle:   1 << 30,
		claudeBin:   "claude",
		workDir:     "",
		path:        "/usr/bin:/bin",
		apiURL:      "https://api.github.com",
		cloneBase:   "https://github.com/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadConfigReadsEveryVariable(t *testing.T) {
	got, err := loadConfig(with(map[string]string{
		"SENTRY_GITHUB_REPOS":   " 1=acme/web , 42=Other-Org/api.v2_x ",
		"CLAUDE_PLUGIN_DIRS":    "/p/codex, /p/auto-task",
		"INVESTIGATION_TIMEOUT": "90s",
		"INVESTIGATION_MAX_AGE": "15m",
		"CLAUDE_BIN":            "/opt/claude",

		"INVESTIGATION_MAX_BUNDLE_BYTES": "4096",
		"INVESTIGATION_WORK_DIR":         "/var/underling",
		"INVESTIGATION_DENY_READ":        " /srv/data , /etc/underling/ ",
		"HOME":                           "/home/underling/",
		"GITHUB_API_URL":                 "http://127.0.0.1:8080/api/",
		"GITHUB_CLONE_URL":               "/srv/remotes/",
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
	if want := []string{"/srv/data", "/etc/underling"}; !reflect.DeepEqual(got.denyRead, want) || got.parentHome != "/home/underling" {
		t.Errorf("denyRead = %q and parentHome = %q, want %q and /home/underling", got.denyRead, got.parentHome, want)
	}
	if got.apiURL != "http://127.0.0.1:8080/api/" || got.cloneBase != "/srv/remotes/" {
		t.Errorf("apiURL = %q and cloneBase = %q, want http://127.0.0.1:8080/api/ and /srv/remotes/", got.apiURL, got.cloneBase)
	}
	if got.maxBundle != 4096 {
		t.Errorf("maxBundle = %d, want 4096", got.maxBundle)
	}
	if got.timeout != 90*time.Second || got.maxAge != 15*time.Minute || got.claudeBin != "/opt/claude" || got.workDir != "/var/underling" {
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
		{"INVESTIGATION_MAX_BUNDLE_BYTES", "0"},
		{"INVESTIGATION_MAX_BUNDLE_BYTES", "-1"},
		{"INVESTIGATION_MAX_BUNDLE_BYTES", "1GiB"},
		{"INVESTIGATION_MAX_BUNDLE_BYTES", "1.5"},
		{"INVESTIGATION_DENY_READ", "relative/dir"},
		{"INVESTIGATION_DENY_READ", "/a,,/b"},
		{"GITHUB_API_URL", "api.github.com"},
		{"GITHUB_API_URL", "ftp://h"},
		{"GITHUB_API_URL", "https://"},
		{"GITHUB_API_URL", "/relative"},
		{"GITHUB_API_URL", "https://u:p@h"},
		{"GITHUB_API_URL", "https://h/api?x=1"},
		{"GITHUB_API_URL", "https://h/api#x"},
		{"GITHUB_CLONE_URL", "https://github.com"},
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

func TestLoadConfigCapsTheBundleLimitBelowTheLargestInt64(t *testing.T) {
	// underling reads one byte past the limit to notice a larger bundle.
	_, err := loadConfig(with(map[string]string{"INVESTIGATION_MAX_BUNDLE_BYTES": "9223372036854775807"}))
	if want := "INVESTIGATION_MAX_BUNDLE_BYTES: 9223372036854775807 is larger than the most, 9223372036854775806"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("got error %v, want %q", err, want)
	}

	got, err := loadConfig(with(map[string]string{"INVESTIGATION_MAX_BUNDLE_BYTES": "9223372036854775806"}))
	if err != nil || got.maxBundle != math.MaxInt64-1 {
		t.Errorf("got maxBundle %d and error %v, want the largest value accepted", got.maxBundle, err)
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

// bedrockEnv is validEnv in Bedrock mode: no Anthropic or OpenAI keys.
func bedrockEnv(overrides map[string]string) func(string) string {
	env := validEnv()
	delete(env, "ANTHROPIC_API_KEY")
	delete(env, "OPENAI_API_KEY")
	maps.Copy(env, map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_REGION":              "eu-central-1",
		"ANTHROPIC_MODEL":         "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"CODEX_BEDROCK_MODEL":     "openai.gpt-oss-120b",
	})
	maps.Copy(env, overrides)
	return func(key string) string { return env[key] }
}

func TestLoadConfigInBedrockModeDefaultsTheSmallModelAndCodexRegion(t *testing.T) {
	got, err := loadConfig(bedrockEnv(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := &bedrockConfig{
		region:         "eu-central-1",
		model:          "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		smallFastModel: "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		codexModel:     "openai.gpt-oss-120b",
		codexRegion:    "eu-central-1",
	}
	if !reflect.DeepEqual(got.bedrock, want) {
		t.Fatalf("bedrock = %+v, want %+v", got.bedrock, want)
	}
	if got.anthropic != (claude.Credential{}) || got.openAIKey != "" {
		t.Errorf("got anthropic %+v and openAIKey %q, want neither", got.anthropic, got.openAIKey)
	}
}

func TestLoadConfigInBedrockModeReadsTheOptionalVariables(t *testing.T) {
	got, err := loadConfig(bedrockEnv(map[string]string{
		"ANTHROPIC_SMALL_FAST_MODEL": "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		"CODEX_BEDROCK_REGION":       "us-west-2",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.bedrock.smallFastModel != "eu.anthropic.claude-haiku-4-5-20251001-v1:0" || got.bedrock.codexRegion != "us-west-2" {
		t.Fatalf("bedrock = %+v", got.bedrock)
	}
}

func TestLoadConfigOutsideBedrockModeIgnoresTheBedrockVariables(t *testing.T) {
	for _, value := range []string{"", "unset"} {
		overrides := map[string]string{"AWS_REGION": "not a region", "ANTHROPIC_MODEL": "bad model!", "CODEX_BEDROCK_REGION": "x"}
		if value != "unset" {
			overrides["CLAUDE_CODE_USE_BEDROCK"] = value
		}
		got, err := loadConfig(with(overrides))
		if err != nil || got.bedrock != nil {
			t.Fatalf("CLAUDE_CODE_USE_BEDROCK %s: got bedrock %+v and error %v, want neither", value, got.bedrock, err)
		}
	}
}

func TestLoadConfigInBedrockModeRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		variable string
		value    string
	}{
		{"CLAUDE_CODE_USE_BEDROCK", "true"},
		{"CLAUDE_CODE_USE_BEDROCK", "0"},
		{"AWS_REGION", ""},
		{"AWS_REGION", "EU-CENTRAL-1"},
		{"AWS_REGION", "eu-central"},
		{"AWS_REGION", "eu_central_1"},
		{"AWS_REGION", "eu-central-1.evil.com/"},
		{"AWS_REGION", "-eu-1"},
		{"ANTHROPIC_MODEL", ""},
		{"ANTHROPIC_MODEL", "   "},
		{"ANTHROPIC_MODEL", "model with space"},
		{"ANTHROPIC_MODEL", "model\"quote"},
		{"CODEX_BEDROCK_MODEL", ""},
		{"CODEX_BEDROCK_MODEL", "a\nb"},
		{"ANTHROPIC_SMALL_FAST_MODEL", "bad model"},
		{"CODEX_BEDROCK_REGION", "us-west"},
		{"ANTHROPIC_API_KEY", "anthropic-key"},
		{"CLAUDE_CODE_OAUTH_TOKEN", "oauth-token"},
		{"OPENAI_API_KEY", "openai-key"},
	}
	for _, tt := range tests {
		t.Run(tt.variable+"="+tt.value, func(t *testing.T) {
			_, err := loadConfig(bedrockEnv(map[string]string{tt.variable: tt.value}))
			if err == nil || !strings.Contains(err.Error(), tt.variable) {
				t.Fatalf("got error %v, want one naming %s", err, tt.variable)
			}
		})
	}
}
