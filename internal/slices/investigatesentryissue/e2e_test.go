package investigatesentryissue_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"heinzel/internal/events"
	"heinzel/internal/platform/app"
	"heinzel/internal/platform/eventhandler"
	"heinzel/internal/platform/eventstore"
	"heinzel/internal/slices/investigatesentryissue"
)

// These tests run the slice end to end, as New wires it: real git against
// a local bare remote, the test binary as a fake claude, and a fake GitHub
// API. The investigator's decisions are specified in investigator_test.go;
// these only check that the real parts fit together.

const (
	githubToken  = "ghp-test-token-4f2a9c"
	sentrySecret = "sentry-test-secret-77b1"
	issueID      = "1117540176"
	branch       = "heinzel/sentry-" + issueID
	apiBase      = "https://api.github.test"
	prURL        = "https://github.com/acme/web/pull/7"
)

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// fakeGitHub is an in-memory GitHub API: it records each request and
// answers with status and body.
type fakeGitHub struct {
	mu       sync.Mutex
	status   int
	body     string
	requests []recordedRequest
}

type recordedRequest struct {
	method, url string
	header      http.Header
	body        []byte
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, recordedRequest{method: r.Method, url: r.URL.String(), header: r.Header.Clone(), body: body})
	return &http.Response{
		StatusCode: g.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(g.body)),
		Request:    r,
	}, nil
}

func (g *fakeGitHub) recorded() []recordedRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// harness is one investigator wired to a fake claude, a local bare remote
// and a fake GitHub.
type harness struct {
	t       *testing.T
	store   *eventstore.Store
	handler eventhandler.Handler
	cancel  context.CancelFunc
	remotes string // the clone URL base: <remotes>/<owner>/<repo>.git
	workDir string
	github  *fakeGitHub
	logs    *bytes.Buffer
	logMu   *sync.Mutex
	// clock is what Params.Now returns; it starts at now.
	clock time.Time
	// automation, ctx and storeDir let a test restart the store and the
	// handler.
	automation app.Automation
	ctx        context.Context
	storeDir   string
}

type option func(env map[string]string)

func withEnv(key, value string) option {
	return func(env map[string]string) { env[key] = value }
}

func newHarness(t *testing.T, mode string, opts ...option) *harness {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	h := &harness{t: t, remotes: t.TempDir(), workDir: t.TempDir(), logs: new(bytes.Buffer), logMu: new(sync.Mutex), clock: now}
	newRemote(t, filepath.Join(h.remotes, "acme", "web.git"))
	if err := os.Chmod(h.workDir, 0o700); err != nil { // as heinzel requires
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.workDir, fakeModeFile), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"SENTRY_GITHUB_REPOS":    "1=acme/web",
		"GITHUB_TOKEN":           githubToken,
		"SENTRY_CLIENT_SECRET":   sentrySecret,
		"ANTHROPIC_API_KEY":      "anthropic-test-key",
		"OPENAI_API_KEY":         "openai-test-key",
		"CLAUDE_PLUGIN_DIRS":     "/plugins/codex,/plugins/auto-task",
		"CLAUDE_BIN":             self,
		"INVESTIGATION_WORK_DIR": h.workDir,
		"PATH":                   os.Getenv("PATH"),
		"HEINZEL_UNRELATED":      "unrelated-value",
		"GITHUB_API_URL":         apiBase,
		"GITHUB_CLONE_URL":       h.remotes + "/",
	}
	for _, opt := range opts {
		opt(env)
	}
	h.github = &fakeGitHub{status: http.StatusCreated, body: `{"number":7,"html_url":"` + prURL + `"}`}

	h.automation, err = investigatesentryissue.New(investigatesentryissue.Params{
		Getenv:     func(key string) string { return env[key] },
		Logger:     slog.New(slog.NewJSONHandler(lockedWriter{h.logMu, h.logs}, nil)),
		HTTPClient: &http.Client{Transport: h.github},
		Now:        func() time.Time { return h.clock },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.storeDir = t.TempDir()
	h.store, err = eventstore.Open(h.storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = h.store.Close() })
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	h.handler = h.automation.Handler(h.ctx, h.store)
	return h
}

// restart closes and reopens the store and builds a new handler on it, as
// a heinzel restart would.
func (h *harness) restart() {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("close store: %v", err)
	}
	var err error
	if h.store, err = eventstore.Open(h.storeDir); err != nil {
		h.t.Fatalf("reopen store: %v", err)
	}
	h.handler = h.automation.Handler(h.ctx, h.store)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// git runs git in dir and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRemote creates a bare repo at path whose main branch has one commit.
func newRemote(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	git(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "Initial commit")
	git(t, seed, "push", "-q", path, "main")
}

func (h *harness) remote() string { return filepath.Join(h.remotes, "acme", "web.git") }

// alert returns a stored SentryIssueAlertReceived for project at ts.
func alert(position uint64, project int64, ts time.Time) eventstore.Event {
	payload, _ := json.Marshal(events.SentryIssueAlertReceived{
		EventID:        "aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4",
		IssueID:        issueID,
		Project:        project,
		Title:          "ReferenceError: blooopy is not defined",
		Level:          "error",
		WebURL:         "https://sentry.io/organizations/test-org/issues/" + issueID + "/",
		TriggeredRule:  "rule-a",
		Culprit:        "?(runner)",
		ExceptionType:  "ReferenceError",
		ExceptionValue: "blooopy is not defined",
		Frames:         []events.SentryStackFrame{{File: "example.js", Function: "runner", Line: 3}},
	})
	return eventstore.Event{
		Stream:    "sentry-issue-alert-x",
		Version:   1,
		Position:  position,
		Type:      events.SentryIssueAlertReceivedType,
		Timestamp: ts,
		Payload:   payload,
	}
}

// investigation returns the events on the issue's investigation stream.
func (h *harness) investigation(id string) []eventstore.Event {
	h.t.Helper()
	all, err := h.store.ReadAfter(0, 1000)
	if err != nil {
		h.t.Fatalf("read events: %v", err)
	}
	var stream []eventstore.Event
	for _, e := range all {
		if e.Stream == "sentry-issue-investigation-"+id {
			stream = append(stream, e)
		}
	}
	return stream
}

// assertStream checks the stream's event types and decodes the last
// payload into last.
func (h *harness) assertStream(wantTypes []string, last any) {
	h.t.Helper()
	stream := h.investigation(issueID)
	var types []string
	for _, e := range stream {
		types = append(types, e.Type)
	}
	if !slices.Equal(types, wantTypes) {
		h.t.Fatalf("stream holds %v, want %v; logs:\n%s", types, wantTypes, h.logText())
	}
	if last != nil {
		if err := json.Unmarshal(stream[len(stream)-1].Payload, last); err != nil {
			h.t.Fatalf("decode %s: %v", stream[len(stream)-1].Type, err)
		}
	}
}

func assertFailed(t *testing.T, h *harness, stage, reasonPart string) {
	t.Helper()
	var f events.SentryIssueInvestigationFailed
	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationFailedType), &f)
	if f.IssueID != issueID || f.Stage != stage || !strings.Contains(f.Reason, reasonPart) {
		t.Errorf("got %+v, want stage %s with a reason containing %q", f, stage, reasonPart)
	}
}

func (h *harness) logText() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logs.String()
}

func (h *harness) logLines() []map[string]any {
	h.t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logText()), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil {
			lines = append(lines, m)
		}
	}
	return lines
}

// assertNoLeftovers checks that only the fake's files remain in the work dir.
func (h *harness) assertNoLeftovers() {
	h.t.Helper()
	entries, err := os.ReadDir(h.workDir)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != fakeModeFile && e.Name() != fakeCallsFile {
			h.t.Errorf("work dir still holds %s", e.Name())
		}
	}
}

var startedThenOutcome = func(outcome string) []string {
	return []string{events.SentryIssueInvestigationStartedType, outcome}
}

func TestHappyPathPushesTheBranchAndOpensADraftPR(t *testing.T) {
	h := newHarness(t, modeHappy)

	h.handler.Handle(alert(5, 1, now))

	var opened events.SentryIssueInvestigationPullRequestOpened
	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationPullRequestOpenedType), &opened)
	if opened != (events.SentryIssueInvestigationPullRequestOpened{IssueID: issueID, PRURL: prURL}) {
		t.Errorf("got %+v", opened)
	}
	var started events.SentryIssueInvestigationStarted
	if err := json.Unmarshal(h.investigation(issueID)[0].Payload, &started); err != nil {
		t.Fatal(err)
	}
	if started != (events.SentryIssueInvestigationStarted{IssueID: issueID, Project: 1, Repo: "acme/web", Branch: branch}) {
		t.Errorf("got %+v", started)
	}

	if got := git(t, h.remote(), "log", "-1", "--format=%s", "refs/heads/"+branch); got != "Fix blooopy" {
		t.Errorf("remote branch tip is %q, want the fake's commit", got)
	}
	if got := git(t, h.remote(), "rev-list", "--count", "main.."+branch); got != "1" {
		t.Errorf("branch is %s commits ahead of main, want 1", got)
	}

	reqs := h.github.recorded()
	if len(reqs) != 1 {
		t.Fatalf("got %d GitHub requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.method != http.MethodPost || req.url != apiBase+"/repos/acme/web/pulls" || req.header.Get("Authorization") != "Bearer "+githubToken {
		t.Errorf("got %s %s with Authorization %q", req.method, req.url, req.header.Get("Authorization"))
	}
	var pr struct {
		Title, Head, Base, Body string
		Draft                   bool
	}
	if err := json.Unmarshal(req.body, &pr); err != nil {
		t.Fatalf("decode PR request %s: %v", req.body, err)
	}
	if pr.Title != "Fix Sentry issue "+issueID+": ReferenceError: blooopy is not defined" || pr.Head != branch || pr.Base != "main" || !pr.Draft {
		t.Errorf("got PR request %+v", pr)
	}
	for _, want := range []string{"https://sentry.io/organizations/test-org/issues/" + issueID + "/", issueID, fakeSummary} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, pr.Body)
		}
	}

	calls := fakeCalls(t, h.workDir)
	if len(calls) != 1 {
		t.Fatalf("claude ran %d times, want 1", len(calls))
	}
	if len(calls[0].TokenFiles) != 0 {
		t.Errorf("the GitHub token is on disk in the run dir: %v", calls[0].TokenFiles)
	}
	for _, form := range tokenForms(githubToken) {
		if got := git(t, h.remote(), "config", "--list"); strings.Contains(got, form) {
			t.Errorf("remote config holds the token as %q", form)
		}
	}
	h.assertNoLeftovers()
}

func TestChildEnvironmentHoldsNoHeinzelSecrets(t *testing.T) {
	for _, mode := range []struct {
		name       string
		opts       []option
		credential string // the variable the child logs in with
		value      string
	}{
		{"api key", nil, "ANTHROPIC_API_KEY", "anthropic-test-key"},
		{"oauth token", []option{withEnv("ANTHROPIC_API_KEY", ""), withEnv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-test-token")}, "CLAUDE_CODE_OAUTH_TOKEN", "oauth-test-token"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			assertChildEnvironment(t, newHarness(t, modeNoop, mode.opts...), mode.credential, mode.value)
		})
	}
}

func assertChildEnvironment(t *testing.T, h *harness, credential, value string) {
	t.Helper()

	h.handler.Handle(alert(5, 1, now))

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	calls := fakeCalls(t, h.workDir)
	if len(calls) != 1 {
		t.Fatalf("claude ran %d times, want 1", len(calls))
	}
	env := map[string]string{}
	var keys []string
	for _, kv := range calls[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{credential, "CLAUDE_CONFIG_DIR", "HOME", "OPENAI_API_KEY", "PATH"}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("child env has %v, want exactly %v", keys, want)
	}
	for _, secret := range []string{githubToken, sentrySecret, "unrelated-value"} {
		if strings.Contains(strings.Join(calls[0].Env, "\n"), secret) {
			t.Errorf("child env contains %q", secret)
		}
	}
	if env[credential] != value || env["OPENAI_API_KEY"] != "openai-test-key" || env["PATH"] != os.Getenv("PATH") {
		t.Errorf("child env = %v", env)
	}
	dir := calls[0].Dir
	for _, own := range []string{env["HOME"], env["CLAUDE_CONFIG_DIR"]} {
		if own == "" || strings.HasPrefix(own, dir) || !strings.HasPrefix(own, filepath.Dir(dir)) {
			t.Errorf("child dir %q should be a per-run dir beside the clone %s", own, dir)
		}
	}
	if !slices.Contains(calls[0].Args, "--plugin-dir") || slices.ContainsFunc(calls[0].Args, func(a string) bool { return strings.Contains(a, "auto-task ") }) {
		t.Errorf("unexpected args %q", calls[0].Args)
	}
	if !strings.HasPrefix(calls[0].Prompt, "/at:auto-task ") || !strings.Contains(calls[0].Prompt, `"issueId": "`+issueID+`"`) {
		t.Errorf("claude got prompt %q on stdin", calls[0].Prompt)
	}
	h.assertNoLeftovers()
}

func TestARepeatedAlertAfterARestartStartsNoSecondInvestigation(t *testing.T) {
	h := newHarness(t, modeNoop)
	h.handler.Handle(alert(5, 1, now))

	h.restart()
	h.handler.Handle(alert(9, 1, now))

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	if calls := fakeCalls(t, h.workDir); len(calls) != 1 {
		t.Errorf("claude ran %d times, want 1", len(calls))
	}
}

func TestALiveAlertHandledLateIsNotStale(t *testing.T) {
	h := newHarness(t, modeNoop) // the handler starts at now
	h.clock = now.Add(3 * time.Hour)

	h.handler.Handle(alert(5, 99, now.Add(time.Minute))) // an unmapped project, so claude doesn't run

	var skipped events.SentryIssueInvestigationSkipped
	h.assertStream([]string{events.SentryIssueInvestigationSkippedType}, &skipped)
	if skipped.Reason != "unmapped" {
		t.Errorf("got %+v, want it skipped as unmapped, not stale", skipped)
	}
}

func TestAnAlertStoredLongBeforeTheHandlerStartedIsStale(t *testing.T) {
	h := newHarness(t, modeNoop) // the handler starts at now
	h.clock = now.Add(3 * time.Hour)

	h.handler.Handle(alert(5, 1, now.Add(-61*time.Minute)))

	var skipped events.SentryIssueInvestigationSkipped
	h.assertStream([]string{events.SentryIssueInvestigationSkippedType}, &skipped)
	if skipped.Reason != "stale" {
		t.Errorf("got %+v", skipped)
	}
	if calls := fakeCalls(t, h.workDir); len(calls) != 0 {
		t.Errorf("claude ran %d times, want 0", len(calls))
	}
}

func TestNewWarnsAboutPATHDirsTheChildMayNotRead(t *testing.T) {
	home := t.TempDir()
	homeBin := filepath.Join(home, "bin")
	h := newHarness(t, modeNoop, withEnv("HOME", home), withEnv("PATH", "/usr/bin:"+homeBin))

	var warned []any
	for _, line := range h.logLines() {
		if line["level"] == "WARN" && strings.Contains(line["msg"].(string), "won't run inside the sandbox") {
			warned = append(warned, line["dir"])
		}
	}
	if !slices.Equal(warned, []any{homeBin}) {
		t.Fatalf("warned about %v, want just %s; logs:\n%s", warned, homeBin, h.logText())
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, err := investigatesentryissue.New(investigatesentryissue.Params{
		Getenv: func(string) string { return "" },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "SENTRY_GITHUB_REPOS") {
		t.Fatalf("got %v, want an error naming SENTRY_GITHUB_REPOS", err)
	}
}

// Compile-time check that New returns the app's automation type.
var _ func(investigatesentryissue.Params) (app.Automation, error) = investigatesentryissue.New
