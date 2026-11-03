package postdeploy

import (
	"encoding/base64"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	return map[string]string{
		"POSTDEPLOY_BASE_URL":          "https://heinzel.example.com",
		"SENTRY_CLIENT_SECRET":         "sentry-secret",
		"POSTDEPLOY_SENTRY_PROJECT_ID": "42",
		"POSTDEPLOY_GITHUB_REPO":       "acme/heinzel-target",
		"POSTDEPLOY_GITHUB_TOKEN":      "github_pat_test",
	}
}

func env(overrides map[string]string) func(string) string {
	e := validEnv()
	maps.Copy(e, overrides)
	return func(key string) string { return e[key] }
}

func TestLoadInputs(t *testing.T) {
	got, err := loadInputs(env(map[string]string{"POSTDEPLOY_BASE_URL": "https://heinzel.example.com/"}))
	if err != nil {
		t.Fatalf("loadInputs: %v", err)
	}
	want := inputs{
		baseURL:  "https://heinzel.example.com",
		secret:   "sentry-secret",
		project:  42,
		repo:     "acme/heinzel-target",
		token:    "github_pat_test",
		logGroup: "/ecs/heinzel",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadInputsTakesTheLogGroup(t *testing.T) {
	got, err := loadInputs(env(map[string]string{"POSTDEPLOY_LOG_GROUP": "/ecs/other"}))
	if err != nil {
		t.Fatalf("loadInputs: %v", err)
	}
	if got.logGroup != "/ecs/other" {
		t.Fatalf("logGroup = %q, want /ecs/other", got.logGroup)
	}
}

func TestLoadInputsNamesEveryMissingVariable(t *testing.T) {
	_, err := loadInputs(func(string) string { return "" })
	if err == nil {
		t.Fatal("loadInputs succeeded with nothing set")
	}
	for _, v := range []string{
		"POSTDEPLOY_BASE_URL", "SENTRY_CLIENT_SECRET", "POSTDEPLOY_SENTRY_PROJECT_ID",
		"POSTDEPLOY_GITHUB_REPO", "POSTDEPLOY_GITHUB_TOKEN",
	} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error doesn't name %s:\n%v", v, err)
		}
	}
}

func TestLoadInputsIgnoresGitHubToken(t *testing.T) {
	_, err := loadInputs(env(map[string]string{"POSTDEPLOY_GITHUB_TOKEN": "", "GITHUB_TOKEN": "ambient"}))
	if err == nil || !strings.Contains(err.Error(), "POSTDEPLOY_GITHUB_TOKEN") {
		t.Fatalf("got %v, want an error naming POSTDEPLOY_GITHUB_TOKEN", err)
	}
}

func TestLoadInputsRejects(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value string
	}{
		{"http base URL", "POSTDEPLOY_BASE_URL", "http://heinzel.example.com"},
		{"relative base URL", "POSTDEPLOY_BASE_URL", "heinzel.example.com"},
		{"base URL with a path", "POSTDEPLOY_BASE_URL", "https://heinzel.example.com/sentry/callback"},
		{"base URL with a query", "POSTDEPLOY_BASE_URL", "https://heinzel.example.com/?x=1"},
		{"base URL with a fragment", "POSTDEPLOY_BASE_URL", "https://heinzel.example.com/#x"},
		{"base URL with user info", "POSTDEPLOY_BASE_URL", "https://u:p@heinzel.example.com"},
		{"base URL without a host", "POSTDEPLOY_BASE_URL", "https:///"},
		{"zero project", "POSTDEPLOY_SENTRY_PROJECT_ID", "0"},
		{"negative project", "POSTDEPLOY_SENTRY_PROJECT_ID", "-3"},
		{"non-numeric project", "POSTDEPLOY_SENTRY_PROJECT_ID", "web"},
		{"repo without owner", "POSTDEPLOY_GITHUB_REPO", "heinzel-target"},
		{"repo with extra part", "POSTDEPLOY_GITHUB_REPO", "acme/heinzel/target"},
		{"repo as URL", "POSTDEPLOY_GITHUB_REPO", "https://github.com/acme/heinzel-target"},
		{"repo dot-dot", "POSTDEPLOY_GITHUB_REPO", "acme/.."},
		{"upstream repo", "POSTDEPLOY_GITHUB_REPO", "hashicorp/go-version"},
		{"upstream repo in other case", "POSTDEPLOY_GITHUB_REPO", "HashiCorp/Go-Version"},
		{"token with whitespace", "POSTDEPLOY_GITHUB_TOKEN", "github_pat test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadInputs(env(map[string]string{tc.variable: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("got %v, want an error naming %s", err, tc.variable)
			}
		})
	}
}

const (
	parent = "1111111111111111111111111111111111111111"
	other  = "2222222222222222222222222222222222222222"
)

func TestParseLsRemote(t *testing.T) {
	got, err := parseLsRemote(parent + "\tHEAD\n" + parent + "\trefs/heads/main\n" + other + "\trefs/pull/1/head\n")
	if err != nil {
		t.Fatalf("parseLsRemote: %v", err)
	}
	want := map[string]string{"HEAD": parent, "refs/heads/main": parent, "refs/pull/1/head": other}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, err := parseLsRemote(""); err != nil || len(got) != 0 {
		t.Fatalf("empty output: got %v, %v", got, err)
	}
	if _, err := parseLsRemote("garbage\n"); err == nil {
		t.Fatal("parseLsRemote accepted a line without a tab")
	}
}

func TestUnexpectedRefs(t *testing.T) {
	refs := map[string]string{
		"HEAD":                        parent,
		"refs/heads/main":             parent,
		"refs/heads/heinzel/sentry-1": other,
		"refs/pull/3/head":            other,
		"refs/pull/3/merge":           other,
		"refs/heads/feature":          other,
		"refs/heads/heinzelx":         other,
		"refs/tags/v1.0.0":            other,
		"refs/tags/v1.0.0^{}":         other,
		"refs/notes/commits":          other,
	}
	got := unexpectedRefs(refs, "main")
	want := []string{"refs/heads/feature", "refs/heads/heinzelx", "refs/notes/commits", "refs/tags/v1.0.0", "refs/tags/v1.0.0^{}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := unexpectedRefs(map[string]string{"refs/heads/master": parent}, "main"); !reflect.DeepEqual(got, []string{"refs/heads/master"}) {
		t.Fatalf("a branch other than the default: got %v", got)
	}
}

func TestHeinzelBranches(t *testing.T) {
	refs := map[string]string{
		"HEAD":                        parent,
		"refs/heads/main":             parent,
		"refs/heads/heinzel/sentry-2": other,
		"refs/heads/heinzel/sentry-1": other,
		"refs/pull/1/head":            other,
	}
	got := heinzelBranches(refs)
	want := []string{"refs/heads/heinzel/sentry-1", "refs/heads/heinzel/sentry-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHasHeads(t *testing.T) {
	if hasHeads(map[string]string{}) || hasHeads(map[string]string{"refs/pull/1/head": other}) {
		t.Error("hasHeads true without a branch")
	}
	if !hasHeads(map[string]string{"refs/heads/main": parent}) {
		t.Error("hasHeads false with a branch")
	}
}

func TestCheckResetRefs(t *testing.T) {
	good := map[string]string{"HEAD": parent, "refs/heads/main": parent, "refs/pull/7/head": other}
	if err := checkResetRefs(good, "main", parent); err != nil {
		t.Fatalf("checkResetRefs: %v", err)
	}
	for name, refs := range map[string]map[string]string{
		"no HEAD":           {"refs/heads/main": parent},
		"no default branch": {"HEAD": parent},
		"default elsewhere": {"HEAD": other, "refs/heads/main": other},
		"heinzel left":      {"HEAD": parent, "refs/heads/main": parent, "refs/heads/heinzel/sentry-1": other},
		"a tag":             {"HEAD": parent, "refs/heads/main": parent, "refs/tags/v1": parent},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkResetRefs(refs, "main", parent); err == nil {
				t.Fatalf("checkResetRefs accepted %v", refs)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	const token = "github_pat_secret"
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	got := redact("plain "+token+" header Basic "+basic, token)
	if strings.Contains(got, token) || strings.Contains(got, basic) {
		t.Fatalf("redact left the token in %q", got)
	}
	if got != "plain [redacted] header Basic [redacted]" {
		t.Fatalf("got %q", got)
	}
	if got := redact("nothing", ""); got != "nothing" {
		t.Fatalf("empty token: got %q", got)
	}
}

func TestBasicAuthHeader(t *testing.T) {
	got := basicAuthHeader("tok")
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRetryable(t *testing.T) {
	limited := http.Header{}
	limited.Set("X-RateLimit-Remaining", "0")
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		body   string
		want   bool
	}{
		{"ok", 200, nil, "[]", false},
		{"server error", 502, nil, "", true},
		{"too many requests", 429, nil, "", true},
		{"primary rate limit", 403, limited, "", true},
		{"secondary rate limit", 403, nil, `{"message":"You have exceeded a secondary rate limit."}`, true},
		{"forbidden", 403, nil, `{"message":"Resource not accessible by personal access token"}`, false},
		{"not found", 404, nil, "", false},
		{"unauthorized", 401, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.status, tc.header, []byte(tc.body)); got != tc.want {
				t.Fatalf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWaitDeadline(t *testing.T) {
	posted := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if got := waitDeadline(posted, time.Time{}, false); !got.Equal(posted.Add(45 * time.Minute)) {
		t.Errorf("no test deadline: got %v", got)
	}
	if got := waitDeadline(posted, posted.Add(time.Hour), true); !got.Equal(posted.Add(45 * time.Minute)) {
		t.Errorf("far test deadline: got %v", got)
	}
	if got := waitDeadline(posted, posted.Add(20*time.Minute), true); !got.Equal(posted.Add(17 * time.Minute)) {
		t.Errorf("near test deadline: got %v", got)
	}
}

func TestPRProblems(t *testing.T) {
	good := pullRequest{Draft: true, State: "open"}
	good.Base.Ref = "main"
	good.Head.Repo = &repoRef{FullName: "acme/target"}
	if got := prProblems(good, "acme/target", "main"); len(got) != 0 {
		t.Fatalf("good PR: %v", got)
	}
	bad := pullRequest{Draft: false, State: "closed"}
	bad.Base.Ref = "dev"
	got := prProblems(bad, "acme/target", "main")
	for _, want := range []string{"draft", "state", "base.ref", "head.repo"} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("problems %v don't mention %s", got, want)
		}
	}
}

func TestTimeoutMessage(t *testing.T) {
	got := timeoutMessage("acme/target", "heinzel/sentry-17", "17", "/ecs/heinzel", 42, 45*time.Minute)
	for _, want := range []string{
		"acme/target", "heinzel/sentry-17", "45m",
		`aws logs tail /ecs/heinzel --since 1h --filter-pattern '"17"'`,
		"unmapped", "42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}
