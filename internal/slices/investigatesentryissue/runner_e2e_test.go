package investigatesentryissue_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"underling/internal/events"
	"underling/internal/slices/investigatesentryissue"
	"underling/internal/slices/investigatesentryissue/contract"
	"underling/internal/slices/investigatesentryissue/runner"
	"underling/internal/slices/investigatesentryissue/runner/runnertest"
)

// These tests run the whole runner exchange in one process, without AWS:
// underling packs, the runner command runs the fake claude, underling
// takes the output and applies its checks. An HTTP file server stands in
// for S3, and the launcher calls the runner's entry point where the ECS
// launcher would start a task.

// localLauncher runs the runner in-process against a file server.
type localLauncher struct {
	t       *testing.T
	files   *runnertest.Files
	workDir string // the runner's own INVESTIGATION_WORK_DIR
	aws     aws.CredentialsProvider
	logs    *bytes.Buffer
	logMu   *sync.Mutex

	mu        sync.Mutex
	exitCodes []int
	released  int
}

// newLocalLauncher returns a launcher whose runner runs the fake claude
// in mode.
func newLocalLauncher(t *testing.T, mode string) *localLauncher {
	t.Helper()
	l := &localLauncher{t: t, files: runnertest.NewFiles(), workDir: t.TempDir(), logs: new(bytes.Buffer), logMu: new(sync.Mutex)}
	t.Cleanup(l.files.Close)
	if err := os.Chmod(l.workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{fakeModeFile: mode, fakeRunnerFile: ""} {
		if err := os.WriteFile(filepath.Join(l.workDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l.aws = &fakeAWS{sets: []aws.Credentials{sessionCredentials("ASIARUNNER", time.Now().Add(2*time.Hour))}}
	return l
}

func (l *localLauncher) Launch(ctx context.Context, run contract.Run, inputBundle, prompt string) (investigatesentryissue.RunnerOutput, error) {
	bundle, err := os.ReadFile(inputBundle)
	if err != nil {
		return investigatesentryissue.RunnerOutput{}, err
	}
	l.files.Put(contract.ObjectKey(run.RunID, contract.InputBundle), bundle)
	l.files.Put(contract.ObjectKey(run.RunID, contract.InputPrompt), []byte(prompt))

	self, err := os.Executable()
	if err != nil {
		return investigatesentryissue.RunnerOutput{}, err
	}
	// The runner's whole environment: the contract, and what the image
	// sets. Nothing of underling's own.
	urls := l.files.URLs(run.RunID)
	env := map[string]string{
		"PATH":                   os.Getenv("PATH"),
		"CLAUDE_BIN":             self,
		"CLAUDE_PLUGIN_DIRS":     "/plugins/codex,/plugins/auto-task",
		"INVESTIGATION_WORK_DIR": l.workDir,
	}
	for _, pair := range (contract.Env{Run: run, URLs: urls}).Pairs() {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	code := runner.Run(ctx, runner.Params{
		Getenv:     func(name string) string { return env[name] },
		Logger:     slog.New(slog.NewJSONHandler(lockedWriter{l.logMu, l.logs}, nil)),
		AWS:        l.aws,
		HTTPClient: &http.Client{},
	})
	l.mu.Lock()
	l.exitCodes = append(l.exitCodes, code)
	l.mu.Unlock()

	get := func(url string) func(context.Context) (io.ReadCloser, error) {
		return func(ctx context.Context) (io.ReadCloser, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("status %d", resp.StatusCode)
			}
			return resp.Body, nil
		}
	}
	return investigatesentryissue.RunnerOutput{
		Secrets: []string{urls.InputBundle, urls.Prompt, urls.OutputBundle, urls.Result},
		Result:  get(urls.Result),
		Bundle:  get(urls.OutputBundle),
		Release: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.released++
		},
	}, nil
}

func (l *localLauncher) logText() string {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	return l.logs.String()
}

// assertRan checks that the runner ran once, exited 0, was released,
// uploaded the bundle before the result and removed its run dir.
func (l *localLauncher) assertRan() {
	l.t.Helper()
	if !slices.Equal(l.exitCodes, []int{0}) || l.released != 1 {
		l.t.Errorf("runner exit codes %v, released %d times; want one exit 0 and one release; runner logs:\n%s", l.exitCodes, l.released, l.logText())
	}
	puts := l.files.Puts()
	if len(puts) != 2 || !strings.HasSuffix(puts[0], "/"+contract.OutputBundle) || !strings.HasSuffix(puts[1], "/"+contract.OutputResult) {
		l.t.Errorf("the runner PUT %q, want the output bundle and then the result", puts)
	}
	entries, err := os.ReadDir(l.workDir)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			l.t.Errorf("the runner left its run dir %s behind", e.Name())
		}
	}
	if logs := l.logText(); strings.Contains(logs, "s3cr3t") {
		l.t.Errorf("the runner's logs show a URL's query:\n%s", logs)
	}
}

// newRunnerHarness is a harness whose investigations run in a local
// runner with the fake claude in mode.
func newRunnerHarness(t *testing.T, mode string, opts ...option) (*harness, *localLauncher) {
	t.Helper()
	l := newLocalLauncher(t, mode)
	underlingAWS := &fakeAWS{err: fmt.Errorf("underling must not fetch AWS credentials for a runner run")}
	h := newHarness(t, mode, append(runnerMode(l, underlingAWS), opts...)...)
	return h, l
}

// assertNothingLeftGitHub checks that no branch was pushed and no PR
// opened.
func assertNothingLeftGitHub(t *testing.T, h *harness) {
	t.Helper()
	if len(h.github.recorded()) != 0 {
		t.Error("a PR was opened")
	}
	if out := git(t, h.remote(), "for-each-ref", "--format=%(refname)"); out != "refs/heads/main" {
		t.Errorf("the remote holds %q, want only main", out)
	}
}

func TestARunnerFixIsPushedAndOpensADraftPR(t *testing.T) {
	h, l := newRunnerHarness(t, modeHappy)

	h.handler.Handle(alert(5, 1, now))

	var opened events.SentryIssueInvestigationPullRequestOpened
	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationPullRequestOpenedType), &opened)
	if opened != (events.SentryIssueInvestigationPullRequestOpened{IssueID: issueID, PRURL: prURL}) {
		t.Errorf("got %+v", opened)
	}
	if got := git(t, h.remote(), "log", "-1", "--format=%s", "refs/heads/"+branch); got != "Fix blooopy" {
		t.Errorf("remote branch tip is %q, want the fake's commit", got)
	}
	if got := git(t, h.remote(), "rev-list", "--count", "main.."+branch); got != "1" {
		t.Errorf("branch is %s commits ahead of main, want 1", got)
	}
	reqs := h.github.recorded()
	if len(reqs) != 1 || !strings.Contains(string(reqs[0].body), fakeSummary) {
		t.Fatalf("got GitHub requests %+v, want one PR with the fake's summary", reqs)
	}
	l.assertRan()
	h.assertNoLeftovers()

	// Claude ran in the runner only, with today's Bedrock environment and
	// nothing of the contract or of underling's secrets.
	if calls := fakeCalls(t, h.workDir); len(calls) != 0 {
		t.Errorf("claude ran in underling's process %d times", len(calls))
	}
	calls := fakeCalls(t, l.workDir)
	if len(calls) != 1 {
		t.Fatalf("claude ran in the runner %d times, want 1", len(calls))
	}
	env, keys := childEnv(t, calls[0])
	want := []string{
		"PATH", "HOME", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "AWS_REGION", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	}
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("claude's env has %v, want exactly %v", keys, want)
	}
	if env["AWS_ACCESS_KEY_ID"] != "ASIARUNNER" || env["AWS_REGION"] != "eu-central-1" || env["ANTHROPIC_MODEL"] != "global.anthropic.claude-sonnet-4-5-20250929-v1:0" {
		t.Errorf("claude's env = %v", env)
	}
	joined := strings.Join(calls[0].Env, "\n")
	for _, secret := range []string{githubToken, sentrySecret, "unrelated-value", l.files.Server.URL, "RUNNER_"} {
		if strings.Contains(joined, secret) {
			t.Errorf("claude's env contains %q", secret)
		}
	}
	if len(calls[0].TokenFiles) != 0 {
		t.Errorf("the GitHub token is on disk in the runner's run dir: %v", calls[0].TokenFiles)
	}
	if calls[0].CodexConfig != wantCodexConfig {
		t.Errorf("codex config =\n%s\nwant\n%s", calls[0].CodexConfig, wantCodexConfig)
	}
	if !strings.HasPrefix(calls[0].Prompt, "/at:auto-task ") || !strings.Contains(calls[0].Prompt, `"issueId": "`+issueID+`"`) {
		t.Errorf("claude got prompt %q on stdin", calls[0].Prompt)
	}
	if filepath.Dir(filepath.Dir(calls[0].Dir)) != resolved(t, l.workDir) {
		t.Errorf("claude ran in %s, not in a run dir of the runner's work dir %s", calls[0].Dir, l.workDir)
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestARunnerRunWithoutCommitsMeansNoChanges(t *testing.T) {
	h, l := newRunnerHarness(t, modeNoop)

	h.handler.Handle(alert(5, 1, now))

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	l.assertRan()
	assertNothingLeftGitHub(t, h)
	h.assertNoLeftovers()
}

func TestARunnerCommitOnAStrayBranchFailsTheInvestigation(t *testing.T) {
	h, l := newRunnerHarness(t, modeStray)

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, "claude", "claude committed on branches other than "+branch+", which underling doesn't push: claude-own")
	l.assertRan()
	assertNothingLeftGitHub(t, h)
	h.assertNoLeftovers()
}

func TestAClaudeFailureInTheRunnerFailsTheInvestigation(t *testing.T) {
	h, l := newRunnerHarness(t, modeFail)

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, "claude", "model refused")
	l.assertRan()
	assertNothingLeftGitHub(t, h)
	h.assertNoLeftovers()
}
