// Package upstreamfix is the real upstream bug both system tests replay:
// Version.Core in hashicorp/go-version panics on a zero-value Version. Fix
// fixed it and added RegressionTest; heinzel gets the repo as it was at
// Fix's parent. The package holds the helpers both tests share: the
// upstream clone, the alert, the precondition, the verdict and the
// comparison. It needs git and go on PATH, and network to clone URL.
package upstreamfix

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	URL            = "https://github.com/hashicorp/go-version.git"
	Fix            = "c6a3b5735e33d3e07a9cfbc8343e84b69d471b80"
	RegressionTest = "TestCoreZeroValue"
)

// Upstream is a local clone of URL.
type Upstream struct {
	Dir       string // the clone
	FixedTest string // version_test.go as of Fix
	Parent    string // the full SHA of Fix's parent
}

// Clone clones URL into a temp dir.
func Clone(t testing.TB) Upstream {
	t.Helper()
	dir := t.TempDir()
	Git(t, "", "clone", "--quiet", URL, dir)
	return Upstream{
		Dir:       dir,
		FixedTest: Git(t, dir, "show", Fix+":version_test.go"),
		Parent:    Git(t, dir, "rev-parse", "--verify", Fix+"^"),
	}
}

// RequireBugReproduces fails the test unless RegressionTest runs and fails
// in dir, a checkout of what heinzel is given. It overwrites dir's
// version_test.go with Fix's.
func RequireBugReproduces(t testing.TB, dir string, u Upstream) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "version_test.go"), u.FixedTest)
	out, err := GoTest(dir, "-v", "-run", "^"+RegressionTest+"$", ".")
	if err == nil || !strings.Contains(out, "--- FAIL: "+RegressionTest) {
		t.Fatalf("%s should run and fail before the fix, got %v:\n%s", RegressionTest, err, out)
	}
}

// CheckFix marks the test failed unless RegressionTest and all of
// go-version's tests pass in dir, a checkout of Claude's branch. It
// overwrites dir's version_test.go with Fix's, so Claude's own test edits
// are dropped on purpose.
func CheckFix(t testing.TB, dir string, u Upstream) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "version_test.go"), u.FixedTest)
	if out, err := GoTest(dir, "-run", "^"+RegressionTest+"$", "."); err != nil {
		t.Errorf("%s fails on Claude's branch: %v\n%s", RegressionTest, err, out)
	}
	if out, err := GoTest(dir, "./..."); err != nil {
		t.Errorf("go-version's tests fail on Claude's branch: %v\n%s", err, out)
	}
}

// LogComparison logs Claude's change to version.go in dir next to
// upstream's fix. It asserts nothing.
func LogComparison(t testing.TB, dir string, u Upstream) {
	t.Helper()
	t.Logf("Claude's fix:\n%s", Git(t, dir, "diff", u.Parent, "HEAD", "--", "version.go"))
	t.Logf("Upstream's fix:\n%s", Git(t, u.Dir, "show", "--format=", Fix, "--", "version.go"))
}

// Alert identifies one Sentry alert for the panic.
type Alert struct {
	EventID string
	IssueID string
	Project int64
}

// PostAlert sends Sentry's event_alert for the panic, as the Go SDK
// reports it, signed with secret, and fails the test unless callbackURL
// answers 200 {"status":"recorded"}.
func PostAlert(t testing.TB, callbackURL, secret string, a Alert) {
	t.Helper()
	const culprit = "github.com/hashicorp/go-version.(*Version).Core"
	frame := map[string]any{"filename": "version.go", "function": culprit, "lineno": 376}
	body, err := json.Marshal(map[string]any{
		"action": "triggered",
		"data": map[string]any{
			"triggered_rule": "New issue",
			"event": map[string]any{
				"event_id": a.EventID,
				"issue_id": a.IssueID,
				"project":  a.Project,
				"title":    "runtime.Error: index out of range [0] with length 0",
				"level":    "fatal",
				"web_url":  "https://sentry.io/organizations/acme/issues/" + a.IssueID + "/",
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
	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", "event_alert")
	req.Header.Set("Sentry-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", callbackURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var reply struct {
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &reply) != nil || reply.Status != "recorded" {
		t.Fatalf("POST %s: %d %s, want 200 {\"status\":\"recorded\"}", callbackURL, resp.StatusCode, raw)
	}
}

// Git runs git in dir ("" for the current dir) and returns its trimmed
// output.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "advice.detachedHead=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// goTestEnv lists the variables GoTest passes on. The tests it runs include
// code Claude wrote, so they get only what the go command needs, not the
// caller's tokens or cloud credentials.
var goTestEnv = []string{
	"PATH", "HOME", "TMPDIR", "GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOPROXY", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOINSECURE",
	"GOSUMDB", "SSL_CERT_FILE", "SSL_CERT_DIR",
}

// GoTest runs go-version's tests in dir with the local toolchain and only
// the goTestEnv variables.
func GoTest(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"test", "-count=1"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"GOTOOLCHAIN=local"}
	for _, k := range goTestEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
