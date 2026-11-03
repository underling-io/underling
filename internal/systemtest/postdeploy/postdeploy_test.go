//go:build postdeploy

package postdeploy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"underling/internal/systemtest/upstreamfix"
)

const (
	githubAPI    = "https://api.github.com"
	pollInterval = 30 * time.Second
	noteInterval = 5 * time.Minute
)

func TestDeployedUnderlingFixesARealUpstreamBug(t *testing.T) {
	in, err := loadInputs(os.Getenv)
	if err != nil {
		t.Fatalf("post-deploy test inputs:\n%v", err)
	}
	// The values live in `in` from here on. Clear them from the environment so
	// no child process inherits them, above all the go test that runs the
	// code Claude wrote.
	for _, k := range []string{"POSTDEPLOY_GITHUB_TOKEN", "SENTRY_CLIENT_SECRET"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
	gh := &gitHub{token: in.token, client: &http.Client{Timeout: time.Minute}}

	repo := guardRepo(t, gh, in.repo)
	u := upstreamfix.Clone(t)
	resetRepo(t, gh, repo, u)

	precondition := t.TempDir()
	gh.git(t, "", "clone", "--quiet", repo.cloneURL(), precondition)
	if head := upstreamfix.Git(t, precondition, "rev-parse", "HEAD"); head != u.Parent {
		t.Fatalf("%s's HEAD is %s after the reset, want %s", repo.FullName, head, u.Parent)
	}
	upstreamfix.RequireBugReproduces(t, precondition, u)

	alert := upstreamfix.Alert{EventID: newEventID(t), IssueID: strconv.FormatInt(time.Now().UnixMilli(), 10), Project: in.project}
	branch := "underling/sentry-" + alert.IssueID
	posted := time.Now()
	upstreamfix.PostAlert(t, in.baseURL+"/sentry/callback", in.secret, alert)
	t.Logf("posted the alert: event %s, issue %s, project %d", alert.EventID, alert.IssueID, alert.Project)

	pr := waitForPR(t, gh, repo, branch, posted, func(waited time.Duration) string {
		return timeoutMessage(repo.FullName, branch, alert.IssueID, in.logGroup, in.project, waited)
	})

	fixed := t.TempDir()
	gh.git(t, "", "clone", "--quiet", "--branch", branch, repo.cloneURL(), fixed)
	if head := upstreamfix.Git(t, fixed, "rev-parse", "HEAD"); head != pr.Head.SHA {
		t.Fatalf("%s is at %s, but PR #%d's head is %s", branch, head, pr.Number, pr.Head.SHA)
	}
	upstreamfix.CheckFix(t, fixed, u)
	upstreamfix.LogComparison(t, fixed, u)
	t.Logf("underling's PR: %s (left open; the next run's reset closes it)", pr.HTMLURL)
}

// repository is what the test reads of the target repo.
type repository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (r repository) cloneURL() string { return "https://github.com/" + r.FullName + ".git" }

// guardRepo fails unless repo is a non-archived, non-fork repo the token
// may push to, and returns it under its canonical name.
func guardRepo(t *testing.T, gh *gitHub, name string) repository {
	t.Helper()
	var repo repository
	gh.getJSON(t, "/repos/"+name, &repo)
	switch {
	case repo.Archived:
		t.Fatalf("%s is archived; the test needs a writable throwaway repo", repo.FullName)
	case repo.Fork:
		t.Fatalf("%s is a fork; the test needs a throwaway repo of its own", repo.FullName)
	case !repo.Permissions.Push:
		t.Fatalf("your GitHub account may not push to %s", repo.FullName)
	case repo.FullName == "" || repo.DefaultBranch == "":
		t.Fatalf("GET /repos/%s: no full_name or default_branch", name)
	}
	return repo
}

// resetRepo makes repo's default branch u.Parent and nothing else, after
// refusing to touch a repo with refs the test doesn't own or with history
// other than go-version's. It closes the open PRs from underling/* branches
// and deletes those branches.
func resetRepo(t *testing.T, gh *gitHub, repo repository, u upstreamfix.Upstream) {
	t.Helper()
	refs := gh.lsRemote(t, repo)
	if bad := unexpectedRefs(refs, repo.DefaultBranch); len(bad) > 0 {
		t.Fatalf("refusing to reset %s: it has refs besides HEAD, %s, underling/* and pull/*: %s",
			repo.FullName, repo.DefaultBranch, strings.Join(bad, ", "))
	}
	defaultRef := "refs/heads/" + repo.DefaultBranch
	if hasHeads(refs) {
		// Only a repo this test reset before (or an empty one) may be reset:
		// underling only ever pushes underling/* branches, so the default branch
		// still sits on go-version's parent of the fix.
		if refs[defaultRef] != u.Parent {
			t.Fatalf("refusing to reset %s: %s is at %q, not go-version's %s; only an empty repo or one this test reset can be reset",
				repo.FullName, repo.DefaultBranch, refs[defaultRef], u.Parent)
		}
		status, _, body, err := gh.do(http.MethodGet, "/repos/"+repo.FullName+"/commits/"+u.Parent, nil)
		if err != nil || status != http.StatusOK {
			t.Fatalf("refusing to reset %s: it doesn't hold go-version's commit %s (GET .../commits: %d %v %s)",
				repo.FullName, u.Parent, status, err, redact(string(body), gh.token))
		}
	}

	for _, pr := range openUnderlingPRs(t, gh, repo) {
		gh.mustDo(t, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo.FullName, pr.Number), map[string]string{"state": "closed"})
		t.Logf("closed %s", pr.HTMLURL)
	}

	refspecs := []string{u.Parent + ":refs/heads/" + repo.DefaultBranch}
	for _, branch := range underlingBranches(refs) {
		refspecs = append(refspecs, ":"+branch)
	}
	// The lease ties the push to the default branch the guard saw (absent
	// in an empty repo), so a change in between fails the push.
	lease := "--force-with-lease=" + defaultRef + ":" + refs[defaultRef]
	gh.git(t, u.Dir, append([]string{"push", "--quiet", lease, "--atomic", "--no-verify", "--no-follow-tags", repo.cloneURL()}, refspecs...)...)

	if err := checkResetRefs(gh.lsRemote(t, repo), repo.DefaultBranch, u.Parent); err != nil {
		t.Fatalf("%s: %v", repo.FullName, err)
	}
	t.Logf("reset %s: %s at %s", repo.FullName, repo.DefaultBranch, u.Parent)
}

// openUnderlingPRs returns repo's open PRs from its own underling/* branches.
func openUnderlingPRs(t *testing.T, gh *gitHub, repo repository) []pullRequest {
	t.Helper()
	var found []pullRequest
	for page := 1; ; page++ {
		var prs []pullRequest
		gh.getJSON(t, fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100&page=%d", repo.FullName, page), &prs)
		for _, pr := range prs {
			if pr.Head.Repo != nil && pr.Head.Repo.FullName == repo.FullName && strings.HasPrefix(pr.Head.Ref, "underling/") {
				found = append(found, pr)
			}
		}
		if len(prs) < 100 {
			return found
		}
	}
}

// waitForPR polls for the PR from branch until the wait deadline, and
// fails unless it is the open draft underling should have opened.
func waitForPR(t *testing.T, gh *gitHub, repo repository, branch string, posted time.Time, onTimeout func(waited time.Duration) string) pullRequest {
	t.Helper()
	testDeadline, hasDeadline := t.Deadline()
	deadline := waitDeadline(posted, testDeadline, hasDeadline)
	owner, _, _ := strings.Cut(repo.FullName, "/")
	path := "/repos/" + repo.FullName + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+branch)
	nextNote := time.Now().Add(noteInterval)
	for {
		wait := min(pollInterval, time.Until(deadline))
		if wait <= 0 {
			t.Fatal(onTimeout(max(deadline.Sub(posted), 0).Round(time.Second)))
		}
		time.Sleep(wait)

		status, header, body, err := gh.do(http.MethodGet, path, nil)
		switch {
		case err != nil:
			t.Logf("GET %s: %v; retrying", path, err)
		case retryable(status, header, body):
			t.Logf("GET %s: %d %s; retrying", path, status, redact(string(body), gh.token))
		case status != http.StatusOK:
			t.Fatalf("GET %s: %d %s", path, status, redact(string(body), gh.token))
		default:
			var prs []pullRequest
			if err := json.Unmarshal(body, &prs); err != nil {
				t.Fatalf("GET %s: %v\n%s", path, err, redact(string(body), gh.token))
			}
			if len(prs) > 0 {
				pr := prs[0]
				if problems := prProblems(pr, repo.FullName, repo.DefaultBranch); len(problems) > 0 {
					t.Fatalf("PR %s from %s isn't underling's draft:\n%s", pr.HTMLURL, branch, strings.Join(problems, "\n"))
				}
				t.Logf("found draft PR %s after %v", pr.HTMLURL, time.Since(posted).Round(time.Second))
				return pr
			}
		}
		if time.Now().After(nextNote) {
			t.Logf("still waiting for a PR from %s (%v since the alert)", branch, time.Since(posted).Round(time.Second))
			nextNote = nextNote.Add(noteInterval)
		}
	}
}

// newEventID returns 32 random lowercase hex characters, as Sentry's.
func newEventID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// gitHub talks to GitHub as the test token: the REST API and git over
// https. The token never goes into argv or a URL, and is redacted from
// failure output.
type gitHub struct {
	token  string
	client *http.Client
}

// do sends one API request; a non-nil body is sent as JSON.
func (g *gitHub) do(method, path string, body any) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, githubAPI+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s", redact(err.Error(), g.token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw, err
}

// mustDo sends one API request and fails the test unless it answers 2xx.
func (g *gitHub) mustDo(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	status, _, raw, err := g.do(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if status < 200 || status > 299 {
		t.Fatalf("%s %s: %d %s", method, path, status, redact(string(raw), g.token))
	}
	return raw
}

// getJSON GETs path and decodes the answer into v.
func (g *gitHub) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw := g.mustDo(t, http.MethodGet, path, nil)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("GET %s: %v\n%s", path, err, redact(string(raw), g.token))
	}
}

// git runs git in dir ("" for the current dir) authenticated as the token
// through an env-only http.extraHeader, ignoring system and global config,
// and returns its trimmed output.
func (g *gitHub) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "advice.detachedHead=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0="+basicAuthHeader(g.token),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, redact(string(out), g.token))
	}
	return strings.TrimSpace(string(out))
}

// lsRemote returns repo's refs.
func (g *gitHub) lsRemote(t *testing.T, repo repository) map[string]string {
	t.Helper()
	refs, err := parseLsRemote(g.git(t, "", "ls-remote", repo.cloneURL()))
	if err != nil {
		t.Fatalf("ls-remote %s: %v", repo.FullName, err)
	}
	return refs
}
