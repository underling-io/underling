package investigatesentryissue_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"underling/internal/events"
	"underling/internal/slices/investigatesentryissue"
	"underling/internal/slices/investigatesentryissue/contract"
)

// These tests run underling's half of the runner exchange against a
// launcher that returns canned output, as a runner that Claude took over
// could: the result and the bundle are untrusted.

func withRunner(l investigatesentryissue.RunnerLauncher) option {
	return func(s *setup) { s.runner = l }
}

// runnerMode is Bedrock mode with l in place of the in-process run. The
// AWS provider fails the test's run if underling asks it for credentials.
func runnerMode(l investigatesentryissue.RunnerLauncher, aws *fakeAWS) []option {
	return append(bedrockMode(aws), withRunner(l))
}

// cannedLauncher answers every launch with the same output.
type cannedLauncher struct {
	err    error
	result func() io.Reader
	bundle func(inputBundle string) io.Reader

	mu       sync.Mutex
	launched []cannedLaunch
	released int
	opened   []string
}

type cannedLaunch struct {
	run         contract.Run
	prompt      string
	inputBundle []byte
}

func (l *cannedLauncher) Launch(_ context.Context, run contract.Run, inputBundle, prompt string) (investigatesentryissue.RunnerOutput, error) {
	data, err := os.ReadFile(inputBundle)
	if err != nil {
		return investigatesentryissue.RunnerOutput{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launched = append(l.launched, cannedLaunch{run: run, prompt: prompt, inputBundle: data})
	if l.err != nil {
		return investigatesentryissue.RunnerOutput{}, l.err
	}
	open := func(name string, r io.Reader) func(context.Context) (io.ReadCloser, error) {
		return func(context.Context) (io.ReadCloser, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.opened = append(l.opened, name)
			return io.NopCloser(r), nil
		}
	}
	out := investigatesentryissue.RunnerOutput{
		Result: open("result", l.result()),
		Release: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.released++
		},
	}
	if l.bundle != nil {
		out.Bundle = open("bundle", l.bundle(inputBundle))
	} else {
		out.Bundle = func(context.Context) (io.ReadCloser, error) { return nil, errors.New("NoSuchKey") }
	}
	return out, nil
}

func resultOf(json string) func() io.Reader {
	return func() io.Reader { return strings.NewReader(json) }
}

// endless never ends, like a chunked answer without a Content-Length.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// timeoutErr reports Timeout, as a net error may.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

const okResult = `{"version":1,"status":"ok","summary":"Fixed it.","reason":""}`

// sameBundle returns the input bundle as the output: no new commits.
func sameBundle(inputBundle string) io.Reader {
	data, err := os.ReadFile(inputBundle)
	if err != nil {
		panic(err)
	}
	return strings.NewReader(string(data))
}

func TestARunnerRunGetsTheContractTheDefaultBranchAndThePrompt(t *testing.T) {
	l := &cannedLauncher{result: resultOf(okResult), bundle: sameBundle}
	aws := &fakeAWS{err: errors.New("underling must not fetch AWS credentials for a runner run")}
	h := newHarness(t, modeNoop, append(runnerMode(l, aws), withEnv("INVESTIGATION_TIMEOUT", "90s"))...)
	git(t, h.remote(), "branch", "other", "main")

	h.handler.Handle(alert(5, 1, now))

	h.assertStream(startedThenOutcome(events.SentryIssueInvestigationNoChangesType), nil)
	if len(l.launched) != 1 || l.released != 1 {
		t.Fatalf("launched %d times and released %d times, want 1 and 1", len(l.launched), l.released)
	}
	got := l.launched[0]
	if len(got.run.RunID) != 32 || strings.Trim(got.run.RunID, "0123456789abcdef") != "" {
		t.Errorf("run id %q is not 32 hex chars", got.run.RunID)
	}
	want := contract.Run{
		RunID: got.run.RunID, Branch: branch, Timeout: 90 * time.Second,
		Bedrock: contract.Bedrock{
			Region:         "eu-central-1",
			Model:          "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
			SmallFastModel: "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
			CodexModel:     "openai.gpt-oss-120b",
			CodexRegion:    "eu-west-1",
		},
	}
	if got.run != want {
		t.Errorf("run = %+v, want %+v", got.run, want)
	}
	if !strings.HasPrefix(got.prompt, "/at:auto-task ") || !strings.Contains(got.prompt, `"issueId": "`+issueID+`"`) {
		t.Errorf("prompt = %q", got.prompt)
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "in.bundle")
	if err := os.WriteFile(bundle, got.inputBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if heads := git(t, dir, "bundle", "list-heads", bundle); strings.Count(heads, "\n") != 1 || !strings.Contains(heads, " HEAD") || !strings.Contains(heads, " refs/heads/main") {
		t.Errorf("the input bundle holds %q, want HEAD and refs/heads/main only", heads)
	}
	if aws.retrieved() != 0 {
		t.Errorf("underling fetched AWS credentials %d times", aws.retrieved())
	}
	if calls := fakeCalls(t, h.workDir); len(calls) != 0 {
		t.Errorf("claude ran in underling's process %d times", len(calls))
	}
	h.assertNoLeftovers()
}

func TestRunnerOutputUnderlingCannotTrustFailsTheRunAsClaude(t *testing.T) {
	garbage := func(string) io.Reader { return strings.NewReader("not a bundle\n") }
	tests := []struct {
		name     string
		launcher *cannedLauncher
		opts     []option
		stage    string
		reason   string
	}{
		{"a launcher error", &cannedLauncher{err: timeoutErr{}}, nil, "claude", "run the runner: i/o timeout"},
		{"a result over 1 MiB", &cannedLauncher{result: resultOf(`{"version":1,"status":"ok","summary":"` + strings.Repeat("x", 1<<20) + `","reason":""}`), bundle: sameBundle}, nil,
			"claude", "the runner's result is larger than 1048576 bytes"},
		{"a result without an end", &cannedLauncher{result: func() io.Reader { return endless{} }, bundle: sameBundle}, nil,
			"claude", "the runner's result is larger than 1048576 bytes"},
		{"a result that is not JSON", &cannedLauncher{result: resultOf(`<Error><Code>NoSuchKey</Code></Error>`)}, nil, "claude", "result is not JSON"},
		{"a result of another version", &cannedLauncher{result: resultOf(`{"version":2,"status":"ok","summary":"","reason":""}`), bundle: sameBundle}, nil,
			"claude", "result has version 2, want 1"},
		{"a result with an unknown status", &cannedLauncher{result: resultOf(`{"version":1,"status":"done","summary":"","reason":""}`), bundle: sameBundle}, nil,
			"claude", `result has unknown status "done"`},
		{"a bundle over the limit", &cannedLauncher{result: resultOf(okResult), bundle: sameBundle}, []option{withEnv("INVESTIGATION_MAX_BUNDLE_BYTES", "64")},
			"claude", "the runner's bundle is larger than 64 bytes (INVESTIGATION_MAX_BUNDLE_BYTES)"},
		{"a bundle without an end", &cannedLauncher{result: resultOf(okResult), bundle: func(string) io.Reader { return endless{} }}, []option{withEnv("INVESTIGATION_MAX_BUNDLE_BYTES", "4096")},
			"claude", "the runner's bundle is larger than 4096 bytes"},
		{"a bundle git cannot verify", &cannedLauncher{result: resultOf(okResult), bundle: garbage}, nil, "claude", "git bundle"},
		{"an ok result without a bundle", &cannedLauncher{result: resultOf(okResult)}, nil, "claude", "download the runner's bundle: NoSuchKey"},
		{"a failed result", &cannedLauncher{result: resultOf(`{"version":1,"status":"failed","summary":"","reason":"claude: exit status 1: model refused"}`), bundle: garbage}, nil,
			"claude", "claude: exit status 1: model refused"},
		{"a timeout result", &cannedLauncher{result: resultOf(`{"version":1,"status":"timeout","summary":"","reason":"claude did not finish within 9h"}`), bundle: garbage}, nil,
			"timeout", "claude did not finish within 30m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, modeNoop, append(runnerMode(tt.launcher, &fakeAWS{err: errors.New("no AWS")}), tt.opts...)...)

			h.handler.Handle(alert(5, 1, now))

			assertFailed(t, h, tt.stage, tt.reason)
			if tt.launcher.err == nil && tt.launcher.released != 1 {
				t.Errorf("released %d times, want 1", tt.launcher.released)
			}
			if len(h.github.recorded()) != 0 {
				t.Error("a PR was opened")
			}
			if out := git(t, h.remote(), "branch", "--list", branch); out != "" {
				t.Errorf("the branch was pushed: %s", out)
			}
			h.assertNoLeftovers()
		})
	}
}

func TestUnderlingReadsTheBundleOnlyForAnOKResult(t *testing.T) {
	l := &cannedLauncher{result: resultOf(`{"version":1,"status":"failed","summary":"","reason":"no clone yet"}`), bundle: sameBundle}
	h := newHarness(t, modeNoop, runnerMode(l, &fakeAWS{err: errors.New("no AWS")})...)

	h.handler.Handle(alert(5, 1, now))

	assertFailed(t, h, "claude", "no clone yet")
	if len(l.opened) != 1 || l.opened[0] != "result" {
		t.Errorf("opened %q, want just the result", l.opened)
	}
}

func TestNewRejectsARunnerOutsideBedrockMode(t *testing.T) {
	env := map[string]string{
		"SENTRY_GITHUB_REPOS": "1=acme/web",
		"GITHUB_TOKEN":        githubToken,
		"ANTHROPIC_API_KEY":   "anthropic-test-key",
		"OPENAI_API_KEY":      "openai-test-key",
		"CLAUDE_PLUGIN_DIRS":  "/plugins/auto-task",
		"PATH":                os.Getenv("PATH"),
	}

	_, err := investigatesentryissue.New(investigatesentryissue.Params{
		Getenv: func(key string) string { return env[key] },
		Logger: slog.New(slog.DiscardHandler),
		Runner: &cannedLauncher{},
	})

	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("got %v, want an error naming CLAUDE_CODE_USE_BEDROCK", err)
	}
}
