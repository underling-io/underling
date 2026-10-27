//go:build system

package systemtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"heinzel/internal/platform/app"
	"heinzel/internal/slices/investigatesentryissue"
	"heinzel/internal/slices/recordsentryissuealert"
)

// The bug: Version.Core panics on a zero-value Version. upstreamFix fixed
// it and added TestCoreZeroValue; heinzel gets the repo as it was just
// before.
const (
	upstreamURL  = "https://github.com/hashicorp/go-version.git"
	upstreamFix  = "c6a3b5735e33d3e07a9cfbc8343e84b69d471b80"
	upstreamTest = "TestCoreZeroValue"

	sentrySecret = "system-test-sentry-secret"
	sentryProj   = 7
	issueID      = "6100000001"
	branch       = "heinzel/sentry-" + issueID
	prURL        = "https://github.com/hashicorp/go-version/pull/1"
)

func TestClaudeFixesARealUpstreamBug(t *testing.T) {
	credential := requireCredentials(t)
	home := os.Getenv("HOME")
	requireToolsOutside(t, home, "go", "node", "codex")
	if _, err := exec.LookPath("claude"); err != nil {
		t.Fatalf("claude is not on PATH: %v", err)
	}

	upstream := t.TempDir()
	git(t, "", "clone", "--quiet", upstreamURL, upstream)
	fixedTest := git(t, upstream, "show", upstreamFix+":version_test.go")
	remotes := t.TempDir()
	remote := filepath.Join(remotes, "hashicorp", "go-version.git")
	serveParentOfFix(t, upstream, remote)
	requireBugReproduces(t, remote, fixedTest)

	github := &fakeGitHub{}
	sink := newLogSink()
	client := startHeinzel(t, sink, github, map[string]string{
		"SENTRY_CLIENT_SECRET":   sentrySecret,
		"SENTRY_GITHUB_REPOS":    strconv.Itoa(sentryProj) + "=hashicorp/go-version",
		"GITHUB_TOKEN":           "system-test-dummy-token",
		credential:               os.Getenv(credential),
		"OPENAI_API_KEY":         os.Getenv("OPENAI_API_KEY"),
		"CLAUDE_PLUGIN_DIRS":     strings.Join(copyPlugins(t, home), ","),
		"INVESTIGATION_TIMEOUT":  "30m",
		"INVESTIGATION_WORK_DIR": privateDir(t),
		"HOME":                   home,
		"PATH":                   os.Getenv("PATH"),
	}, remotes+"/")

	postAlert(t, client)
	finished := sink.waitFor(t, investigationDeadline(t), func(r map[string]any) bool {
		return r["msg"] == "sentry issue investigation finished" && r["issueId"] == issueID
	})
	if finished["outcome"] != "pr_opened" {
		t.Fatalf("investigation ended %v (stage %v): %v", finished["outcome"], finished["stage"], finished["reason"])
	}
	if reqs := github.recorded(); len(reqs) != 1 || reqs[0] != "POST /repos/hashicorp/go-version/pulls" {
		t.Errorf("GitHub got %q, want one draft PR request for hashicorp/go-version", reqs)
	}

	fixed := t.TempDir()
	git(t, "", "clone", "--quiet", "--branch", branch, remote, fixed)
	writeFile(t, filepath.Join(fixed, "version_test.go"), fixedTest)
	if out, err := goTest(fixed, "-run", "^"+upstreamTest+"$", "."); err != nil {
		t.Errorf("%s fails on Claude's branch: %v\n%s", upstreamTest, err, out)
	}
	if out, err := goTest(fixed, "./..."); err != nil {
		t.Errorf("go-version's tests fail on Claude's branch: %v\n%s", err, out)
	}
	t.Logf("Claude's fix:\n%s", git(t, fixed, "diff", upstreamFix+"^", "HEAD", "--", "version.go"))
	t.Logf("Upstream's fix:\n%s", git(t, upstream, "show", "--format=", upstreamFix, "--", "version.go"))
}

// requireCredentials fails unless the Claude and OpenAI credentials are
// set, and returns the variable holding the Claude one.
func requireCredentials(t *testing.T) string {
	t.Helper()
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Fatal("OPENAI_API_KEY must be set: the child runs Codex")
	}
	for _, v := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if os.Getenv(v) != "" {
			return v
		}
	}
	t.Fatal("CLAUDE_CODE_OAUTH_TOKEN (from claude setup-token) or ANTHROPIC_API_KEY must be set")
	return ""
}

// requireToolsOutside fails unless each tool is on PATH outside home: the
// sandboxed child may not read heinzel's HOME.
func requireToolsOutside(t *testing.T, home string, tools ...string) {
	t.Helper()
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve HOME: %v", err)
	}
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
		if path, err = filepath.EvalSymlinks(path); err != nil {
			t.Fatalf("resolve %s: %v", tool, err)
		}
		if strings.HasPrefix(path, resolvedHome+string(filepath.Separator)) {
			t.Fatalf("%s resolves to %s, under HOME, which the sandboxed child can't read; install it elsewhere", tool, path)
		}
	}
}

// copyPlugins copies the newest installed Codex and auto-task plugins out
// of home, where heinzel won't take plugin dirs from, and returns the
// copies.
func copyPlugins(t *testing.T, home string) []string {
	t.Helper()
	dst := t.TempDir()
	var dirs []string
	for _, p := range []struct{ marketplace, name string }{{"openai-codex", "codex"}, {"auto-task", "at"}} {
		installed := filepath.Join(home, ".claude", "plugins", "cache", p.marketplace, p.name)
		entries, err := os.ReadDir(installed)
		if err != nil || len(entries) == 0 {
			t.Fatalf("plugin %s is not installed in %s: %v", p.name, installed, err)
		}
		versions := make([]string, len(entries))
		for i, e := range entries {
			versions[i] = e.Name()
		}
		slices.SortFunc(versions, compareVersions)
		dir := filepath.Join(dst, p.name)
		if err := os.CopyFS(dir, os.DirFS(filepath.Join(installed, versions[len(versions)-1]))); err != nil {
			t.Fatalf("copy plugin %s: %v", p.name, err)
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// compareVersions orders dotted versions numerically, part by part.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		if aErr != nil || bErr != nil {
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
			continue
		}
		if an != bn {
			return an - bn
		}
	}
	return len(as) - len(bs)
}

// serveParentOfFix makes remote a bare repo whose only branch, main, is
// the commit before the fix, so the fix isn't reachable.
func serveParentOfFix(t *testing.T, upstream, remote string) {
	t.Helper()
	git(t, upstream, "branch", "--force", "system-test-base", upstreamFix+"^")
	git(t, "", "init", "--quiet", "--bare", "--initial-branch=main", remote)
	git(t, remote, "fetch", "--quiet", "--no-tags", upstream, "refs/heads/system-test-base:refs/heads/main")
}

// requireBugReproduces fails unless upstream's regression test runs and
// fails on what remote serves.
func requireBugReproduces(t *testing.T, remote, fixedTest string) {
	t.Helper()
	dir := t.TempDir()
	git(t, "", "clone", "--quiet", remote, dir)
	writeFile(t, filepath.Join(dir, "version_test.go"), fixedTest)
	out, err := goTest(dir, "-v", "-run", "^"+upstreamTest+"$", ".")
	if err == nil || !strings.Contains(out, "--- FAIL: "+upstreamTest) {
		t.Fatalf("%s should run and fail before the fix, got %v:\n%s", upstreamTest, err, out)
	}
}

// startHeinzel runs heinzel's Sentry slices on an in-memory listener, with
// GitHub faked and repos cloned from cloneBase. It returns a client
// connected to it.
func startHeinzel(t *testing.T, sink *logSink, github *fakeGitHub, env map[string]string, cloneBase string) *http.Client {
	t.Helper()
	getenv := func(key string) string { return env[key] }
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(sink, os.Stderr), nil))
	record, err := recordsentryissuealert.New(getenv, logger)
	if err != nil {
		t.Fatalf("record slice: %v", err)
	}
	investigate, err := investigatesentryissue.New(investigatesentryissue.Params{
		Getenv:         getenv,
		Logger:         logger,
		GitHubAPIURL:   "https://api.github.test",
		GitHubCloneURL: cloneBase,
		HTTPClient:     &http.Client{Transport: github},
		Now:            time.Now,
	})
	if err != nil {
		t.Fatalf("investigate slice: %v", err)
	}

	ln := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	cfg := app.Config{DataDir: t.TempDir(), ShutdownTimeout: 10 * time.Second}
	slices := app.Slices{StateChanges: []app.StateChange{record}, Automations: []app.Automation{investigate}}
	go func() { runErr <- app.Run(ctx, cfg, ln, slices, logger) }()
	t.Cleanup(func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("heinzel stopped with %v", err)
		}
	})
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{DialContext: ln.Dial, DisableKeepAlives: true}}
}

// postAlert sends Sentry's alert for the panic, as the Go SDK reports it.
func postAlert(t *testing.T, client *http.Client) {
	t.Helper()
	const culprit = "github.com/hashicorp/go-version.(*Version).Core"
	frame := map[string]any{"filename": "version.go", "function": culprit, "lineno": 376}
	body, err := json.Marshal(map[string]any{
		"action": "triggered",
		"data": map[string]any{
			"triggered_rule": "New issue",
			"event": map[string]any{
				"event_id": "5f1c0e3a9b2d4c7e8a6b1d0f2e3c4b5a",
				"issue_id": issueID,
				"project":  sentryProj,
				"title":    "runtime.Error: index out of range [0] with length 0",
				"level":    "fatal",
				"web_url":  "https://sentry.io/organizations/acme/issues/" + issueID + "/",
				"culprit":  culprit,
				"exception": map[string]any{"values": []any{map[string]any{
					"type":       "runtime.Error",
					"value":      "index out of range [0] with length 0",
					"stacktrace": map[string]any{"frames": []any{frame}},
				}}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://heinzel.test/sentry/callback", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(sentrySecret))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", "event_alert")
	req.Header.Set("Sentry-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /sentry/callback: %v", err)
	}
	defer resp.Body.Close()
	if raw, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sentry/callback: %d %s", resp.StatusCode, raw)
	}
}

// investigationDeadline leaves a minute of the test's own timeout for the
// checks after the investigation.
func investigationDeadline(t *testing.T) time.Time {
	if d, ok := t.Deadline(); ok {
		return d.Add(-time.Minute)
	}
	return time.Now().Add(44 * time.Minute)
}

// privateDir returns a new dir only this user may use, as heinzel requires
// of its work dir.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// git runs git in dir ("" for the current dir) and returns its trimmed
// output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "advice.detachedHead=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// goTest runs go-version's tests in dir with the local toolchain.
func goTest(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"test", "-count=1"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// fakeGitHub answers every request as a created draft PR and records it.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []string
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	g.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"number":1,"html_url":"` + prURL + `"}`)),
		Request:    r,
	}, nil
}

func (g *fakeGitHub) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// logSink collects JSON log records, one per Write, and signals each one.
type logSink struct {
	mu      sync.Mutex
	records []map[string]any
	changed chan struct{} // closed and replaced on each record
}

func newLogSink() *logSink { return &logSink{changed: make(chan struct{})} }

func (s *logSink) Write(p []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(p, &record); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	close(s.changed)
	s.changed = make(chan struct{})
	return len(p), nil
}

// waitFor returns the first record matching match, failing at deadline.
func (s *logSink) waitFor(t *testing.T, deadline time.Time, match func(map[string]any) bool) map[string]any {
	t.Helper()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	for seen := 0; ; {
		s.mu.Lock()
		records, changed := s.records, s.changed
		s.mu.Unlock()
		for ; seen < len(records); seen++ {
			if match(records[seen]) {
				return records[seen]
			}
		}
		select {
		case <-changed:
		case <-timeout.C:
			t.Fatalf("no matching log record by %v", deadline)
		}
	}
}

// pipeListener is an in-memory net.Listener: Dial hands it the server end
// of a net.Pipe, so heinzel is served without binding a socket.
type pipeListener struct {
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_, _ = client.Close(), server.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_, _ = client.Close(), server.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
