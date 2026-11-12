package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/aws/aws-sdk-go-v2/aws"

	"underling/internal/slices/investigatesentryissue/contract"
	"underling/internal/slices/investigatesentryissue/runner/runnertest"
)

// The test binary doubles as a fake claude: the runner runs it as
// CLAUDE_BIN, and the fake recognises itself by its second argument, -p.
// Its env holds only what the runner hands the child, so it reads its mode
// from, and records its env to, files in the work dir: two levels above
// its working directory, the clone.
const (
	fakeModeFile    = "fake-claude-mode"
	fakeEnvFile     = "fake-claude-env"
	fakeStartedFile = "fake-claude-started"

	modeCommit = "commit" // commit a file, then report success
	modeEcho   = "echo"   // report the prompt as the summary
	modeFail   = "fail"   // commit a file, then fail
	modeHang   = "hang"   // commit a file, then never finish
	modeNoGit  = "no-git" // delete the clone's .git, then report success

	testRunID  = "run-1"
	testBranch = "underling/sentry-1"
	waitLimit  = 30 * time.Second
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[2] == "-p" {
		os.Exit(fakeClaude())
	}
	os.Exit(m.Run())
}

func fakeClaude() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "fake claude: "+format+"\n", args...)
		return 3
	}
	emit := func(event map[string]any) {
		line, _ := json.Marshal(event)
		fmt.Println(string(line))
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail("getwd: %v", err)
	}
	workDir := filepath.Dir(filepath.Dir(dir))
	mode, err := os.ReadFile(filepath.Join(workDir, fakeModeFile))
	if err != nil {
		return fail("read mode: %v", err)
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("read stdin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, fakeEnvFile), []byte(strings.Join(os.Environ(), "\n")), 0o644); err != nil {
		return fail("record env: %v", err)
	}
	var plugins []map[string]string
	for i, a := range os.Args {
		if a == "--plugin-dir" && i+1 < len(os.Args) {
			plugins = append(plugins, map[string]string{"name": filepath.Base(os.Args[i+1]), "path": os.Args[i+1]})
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "permissionMode": "acceptEdits", "plugins": plugins})
	commit := func() error {
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
			return err
		}
		for _, args := range [][]string{{"add", "fix.txt"}, {"commit", "-q", "-m", "Fix it"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git %v: %v: %s", args, err, out)
			}
		}
		return nil
	}
	switch m := strings.TrimSpace(string(mode)); m {
	case modeEcho:
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": string(prompt)})
		return 0
	case modeNoGit:
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			return fail("remove .git: %v", err)
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Fixed it."})
		return 0
	case modeCommit, modeFail, modeHang:
		if err := commit(); err != nil {
			return fail("%v", err)
		}
		switch m {
		case modeFail:
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "model refused"})
			return 1
		case modeHang:
			if err := os.WriteFile(filepath.Join(workDir, fakeStartedFile), nil, 0o644); err != nil {
				return fail("record start: %v", err)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Fixed it."})
		return 0
	default:
		return fail("unknown mode %q", m)
	}
}

// fixture is one runner run: a file server holding the inputs, a work dir
// and the environment.
type fixture struct {
	t       *testing.T
	files   *runnertest.Files
	workDir string
	env     map[string]string
	logs    *lockedBuffer
	aws     aws.CredentialsProvider
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

func key(name string) string { return contract.ObjectKey(testRunID, name) }

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, files: runnertest.NewFiles(), workDir: t.TempDir(), logs: new(lockedBuffer)}
	t.Cleanup(f.files.Close)
	if err := os.Chmod(f.workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workDir, fakeModeFile), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}

	seed := t.TempDir()
	git(t, seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", "README.md")
	git(t, seed, "commit", "-q", "-m", "Initial commit")
	bundle := filepath.Join(t.TempDir(), "in.bundle")
	git(t, seed, "bundle", "create", "-q", bundle, "HEAD", "refs/heads/main")
	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	f.files.Put(key(contract.InputBundle), data)
	f.files.Put(key(contract.InputPrompt), []byte("the prompt"))

	f.env = map[string]string{
		"PATH":                   os.Getenv("PATH"),
		"CLAUDE_BIN":             self,
		"CLAUDE_PLUGIN_DIRS":     "/plugins/codex, /plugins/auto-task",
		"INVESTIGATION_WORK_DIR": f.workDir,
		"GITHUB_TOKEN":           "must-not-reach-claude",
	}
	contractEnv := contract.Env{
		Run: contract.Run{
			RunID: testRunID, Branch: testBranch, Timeout: time.Minute,
			Bedrock: contract.Bedrock{Region: "eu-central-1", Model: "claude-model", SmallFastModel: "claude-small-model", CodexModel: "codex-model", CodexRegion: "eu-west-1"},
		},
		URLs: f.files.URLs(testRunID),
	}
	for _, pair := range contractEnv.Pairs() {
		name, value, _ := strings.Cut(pair, "=")
		f.env[name] = value
	}
	f.aws = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "aws-secret", SessionToken: "aws-session"}, nil
	})
	return f
}

// run runs the runner to its end and returns its exit code.
func (f *fixture) run(ctx context.Context) int {
	f.t.Helper()
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, Params{
			Getenv:     func(name string) string { return f.env[name] },
			Logger:     slog.New(slog.NewJSONHandler(f.logs, nil)),
			AWS:        f.aws,
			HTTPClient: &http.Client{},
			retryDelay: time.Millisecond,
		})
	}()
	select {
	case code := <-done:
		return code
	case <-time.After(waitLimit):
		f.t.Fatal("the runner did not return in time")
		return -1
	}
}

// result returns the uploaded result.
func (f *fixture) result() contract.Result {
	f.t.Helper()
	data, ok := f.files.Get(key(contract.OutputResult))
	if !ok {
		f.t.Fatalf("no result was uploaded; logs:\n%s", f.logs)
	}
	result, err := contract.DecodeResult(data)
	if err != nil {
		f.t.Fatalf("uploaded result %s: %v", data, err)
	}
	return result
}

// outputBranches returns the branches in the uploaded bundle with their
// commit subjects.
func (f *fixture) outputBranches() map[string]string {
	f.t.Helper()
	data, ok := f.files.Get(key(contract.OutputBundle))
	if !ok {
		f.t.Fatalf("no bundle was uploaded; logs:\n%s", f.logs)
	}
	dir := f.t.TempDir()
	bundle := filepath.Join(dir, "out.bundle")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	git(f.t, dir, "clone", "-q", "--bare", bundle, "out.git")
	branches := map[string]string{}
	for line := range strings.Lines(git(f.t, filepath.Join(dir, "out.git"), "for-each-ref", "--format=%(refname:short) %(subject)", "refs/heads")) {
		name, subject, _ := strings.Cut(strings.TrimSpace(line), " ")
		branches[name] = subject
	}
	return branches
}

// assertNoSecrets fails if the logs or the result show a URL's signature.
func (f *fixture) assertNoSecrets() {
	f.t.Helper()
	if logs := f.logs.String(); strings.Contains(logs, "s3cr3t") {
		f.t.Errorf("the logs show a URL's query:\n%s", logs)
	}
	if data, ok := f.files.Get(key(contract.OutputResult)); ok && strings.Contains(string(data), "s3cr3t") {
		f.t.Errorf("the result shows a URL's query: %s", data)
	}
}

// assertRunDirRemoved fails if the work dir holds more than the fake's
// files.
func (f *fixture) assertRunDirRemoved() {
	f.t.Helper()
	entries, err := os.ReadDir(f.workDir)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			f.t.Errorf("the run dir %s was left behind", e.Name())
		}
	}
}

var wantPuts = []string{key(contract.OutputBundle), key(contract.OutputResult)}

func TestRunUploadsTheBundleAndThenAnOKResult(t *testing.T) {
	f := newFixture(t, modeCommit)

	code := f.run(context.Background())

	if code != 0 {
		t.Fatalf("exit code %d; logs:\n%s", code, f.logs)
	}
	if got, want := f.result(), (contract.Result{Version: 1, Status: contract.StatusOK, Summary: "Fixed it."}); got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if got := f.files.Puts(); !slices.Equal(got, wantPuts) {
		t.Errorf("PUTs = %q, want %q", got, wantPuts)
	}
	if got, want := f.outputBranches(), map[string]string{"main": "Initial commit", testBranch: "Fix it"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("bundle branches = %v, want %v", got, want)
	}
	f.assertRunDirRemoved()
	f.assertNoSecrets()
}

func TestClaudeGetsTodaysBedrockEnvAndNothingOfTheContract(t *testing.T) {
	f := newFixture(t, modeCommit)

	f.run(context.Background())

	data, err := os.ReadFile(filepath.Join(f.workDir, fakeEnvFile))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for line := range strings.Lines(string(data)) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		names = append(names, name)
		if strings.Contains(value, f.files.Server.URL) {
			t.Errorf("claude's %s holds a URL of the run", name)
		}
	}
	slices.Sort(names)
	want := []string{
		"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
		"AWS_ACCESS_KEY_ID", "AWS_REGION", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CONFIG_DIR", "HOME", "PATH",
	}
	if !slices.Equal(names, want) {
		t.Errorf("claude's env has %q, want %q", names, want)
	}
}

func TestAClaudeFailureStillUploadsTheBundleAndAFailedResult(t *testing.T) {
	f := newFixture(t, modeFail)

	code := f.run(context.Background())

	result := f.result()
	if code != 0 || result.Status != contract.StatusFailed || !strings.Contains(result.Reason, "model refused") {
		t.Fatalf("exit code %d, result %+v; want 0 and a failed result with claude's reason", code, result)
	}
	if got := f.files.Puts(); !slices.Equal(got, wantPuts) {
		t.Errorf("PUTs = %q, want %q", got, wantPuts)
	}
	f.assertRunDirRemoved()
}

func TestAClaudeTimeoutUploadsTheBundleAndATimeoutResult(t *testing.T) {
	f := newFixture(t, modeHang)
	f.env[contract.TimeoutEnvVar] = "3s"

	code := f.run(context.Background())

	if got, want := f.result(), (contract.Result{Version: 1, Status: contract.StatusTimeout, Reason: "claude did not finish within 3s"}); code != 0 || got != want {
		t.Fatalf("exit code %d, result %+v; want 0 and %+v", code, got, want)
	}
	if got := f.outputBranches()[testBranch]; got != "Fix it" {
		t.Errorf("the bundle's branch is at %q, want the commit made before the timeout", got)
	}
	f.assertRunDirRemoved()
}

func TestAStoppedRunnerSkipsTheBundleAndUploadsAFailedResult(t *testing.T) {
	f := newFixture(t, modeHang)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The watcher ends with the test: cancel stops it even if the fake
	// never started.
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(f.workDir, fakeStartedFile)); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	code := f.run(ctx)

	if got, want := f.result(), (contract.Result{Version: 1, Status: contract.StatusFailed, Reason: "the runner was stopped before it finished"}); code != 0 || got != want {
		t.Fatalf("exit code %d, result %+v; want 0 and %+v", code, got, want)
	}
	if got, want := f.files.Puts(), []string{key(contract.OutputResult)}; !slices.Equal(got, want) {
		t.Errorf("PUTs = %q, want %q", got, want)
	}
	f.assertRunDirRemoved()
}

func TestWithoutAResultURLTheRunnerNamesEveryMissingVariableAndExitsNonZero(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.env = map[string]string{"PATH": os.Getenv("PATH")}

	code := f.run(context.Background())

	if code == 0 {
		t.Fatal("exit code 0")
	}
	for _, variable := range []string{"RUNNER_RESULT_URL: must be set", "RUNNER_RUN_ID: must be set", "CLAUDE_PLUGIN_DIRS: must be set"} {
		if !strings.Contains(f.logs.String(), variable) {
			t.Errorf("the logs don't say %q:\n%s", variable, f.logs)
		}
	}
}

func TestAnInvalidEnvironmentIsAFailedResult(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.env[contract.BranchEnvVar] = ""
	f.env[contract.PromptURLEnvVar] = "not-a-url?" + runnertest.Signature

	code := f.run(context.Background())

	want := contract.Result{Version: 1, Status: contract.StatusFailed, Reason: "invalid runner environment: RUNNER_BRANCH: must be set\nRUNNER_PROMPT_URL: is not an absolute http(s) URL"}
	if got := f.result(); code != 0 || got != want {
		t.Fatalf("exit code %d, result %+v; want 0 and %+v", code, got, want)
	}
	if got, want := f.files.Puts(), []string{key(contract.OutputResult)}; !slices.Equal(got, want) {
		t.Errorf("PUTs = %q, want %q", got, want)
	}
	f.assertNoSecrets()
}

func TestABranchGitRejectsIsAFailedResult(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.env[contract.BranchEnvVar] = "a..b"

	code := f.run(context.Background())

	if got := f.result(); code != 0 || got.Status != contract.StatusFailed || got.Reason != `"a..b" is not a branch name` {
		t.Fatalf("exit code %d, result %+v", code, got)
	}
	f.assertRunDirRemoved()
}

func TestFailingAWSCredentialsAreAFailedResultNotATimeout(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.aws = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, context.DeadlineExceeded
	})

	code := f.run(context.Background())

	if got := f.result(); code != 0 || got.Status != contract.StatusFailed || !strings.HasPrefix(got.Reason, "get AWS credentials: ") {
		t.Fatalf("exit code %d, result %+v", code, got)
	}
}

func TestARefusedResultUploadExitsNonZeroWithoutShowingTheURL(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.files.Answer(http.MethodPut, key(contract.OutputResult), http.StatusForbidden)

	code := f.run(context.Background())

	if code == 0 {
		t.Fatal("exit code 0 though the result was refused")
	}
	if !strings.Contains(f.logs.String(), "PUT: status 403") {
		t.Errorf("the logs don't say why:\n%s", f.logs)
	}
	f.assertNoSecrets()
	f.assertRunDirRemoved()
}

func TestAnUnreachableServerShowsNoURLEither(t *testing.T) {
	f := newFixture(t, modeCommit)
	dead := runnertest.NewFiles()
	dead.Close()
	f.env[contract.InputBundleURLEnvVar] = dead.URL("in.bundle")
	f.env[contract.ResultURLEnvVar] = dead.URL("result.json")

	code := f.run(context.Background())

	if code == 0 {
		t.Fatal("exit code 0 though nothing could be uploaded")
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "download the input bundle: GET: ") || !strings.Contains(logs, "upload the result") {
		t.Errorf("the logs don't name both failures:\n%s", logs)
	}
	f.assertNoSecrets()
}

func TestARefusedBundleUploadTurnsOKIntoFailed(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.files.Answer(http.MethodPut, key(contract.OutputBundle), http.StatusForbidden)

	code := f.run(context.Background())

	want := contract.Result{Version: 1, Status: contract.StatusFailed, Reason: "upload the output bundle: PUT: status 403"}
	if got := f.result(); code != 0 || got != want {
		t.Fatalf("exit code %d, result %+v; want 0 and %+v", code, got, want)
	}
	f.assertNoSecrets()
}

func TestAnOKWithoutAGitDirInTheCloneIsAFailedResult(t *testing.T) {
	f := newFixture(t, modeNoGit)

	code := f.run(context.Background())

	result := f.result()
	if code != 0 || result.Status != contract.StatusFailed || !strings.HasPrefix(result.Reason, "bundle the work clone's branches: ") {
		t.Fatalf("exit code %d, result %+v; want 0 and a failed result naming the bundle", code, result)
	}
	if got, want := f.files.Puts(), []string{key(contract.OutputResult)}; !slices.Equal(got, want) {
		t.Errorf("PUTs = %q, want %q", got, want)
	}
	f.assertRunDirRemoved()
	f.assertNoSecrets()
}

func TestTransfersAreTriedThreeTimesOnServerErrors(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.files.Answer(http.MethodGet, key(contract.InputBundle), 500, 503, 0)
	f.files.Answer(http.MethodPut, key(contract.OutputBundle), 502, 0)
	f.files.Answer(http.MethodPut, key(contract.OutputResult), 500, 500, 0)

	code := f.run(context.Background())

	if got := f.result(); code != 0 || got.Status != contract.StatusOK {
		t.Fatalf("exit code %d, result %+v; logs:\n%s", code, got, f.logs)
	}

	f = newFixture(t, modeCommit)
	f.files.Answer(http.MethodGet, key(contract.InputPrompt), 500, 500, 500, 0)

	code = f.run(context.Background())

	if got := f.result(); code != 0 || got.Status != contract.StatusFailed || got.Reason != "download the prompt: GET: status 500" {
		t.Fatalf("exit code %d, result %+v", code, got)
	}
}

func TestASummaryNamingARunURLIsRedacted(t *testing.T) {
	f := newFixture(t, modeEcho)
	f.files.Put(key(contract.InputPrompt), []byte("see "+f.env[contract.ResultURLEnvVar]+" and ?"+runnertest.Signature))

	f.run(context.Background())

	if got := f.result(); got.Summary != "see [redacted] and ?[redacted]" {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestAPromptOverTheLimitIsAFailedResult(t *testing.T) {
	f := newFixture(t, modeCommit)
	f.files.Put(key(contract.InputPrompt), bytes.Repeat([]byte("x"), maxPromptBytes+1))

	f.run(context.Background())

	if got := f.result(); got.Status != contract.StatusFailed || got.Reason != "download the prompt: GET: larger than the limit" {
		t.Errorf("result = %+v", got)
	}
}

func TestOutlivingIsCancelledOnlyAfterTheGrace(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := outliving(parent, 50*time.Millisecond)
	defer cancel()

	stop()

	if err := ctx.Err(); err != nil {
		t.Fatalf("cancelled with its parent: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(waitLimit):
		t.Fatal("not cancelled after the grace")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("err = %v", ctx.Err())
	}
}
