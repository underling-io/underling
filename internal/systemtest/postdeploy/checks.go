package postdeploy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The checks below are the test's pure logic: no network, no clock. The
// postdeploy-tagged test does the I/O around them.

const (
	// maxWait bounds the wait for heinzel's draft PR after the alert.
	maxWait = 45 * time.Minute
	// deadlineMargin leaves the verdict this much of go test's -timeout.
	deadlineMargin = 3 * time.Minute

	defaultLogGroup = "/ecs/heinzel"
	upstreamRepo    = "hashicorp/go-version"
	heinzelPrefix   = "refs/heads/heinzel/"
)

// inputs is what the test reads from its environment.
type inputs struct {
	baseURL  string // https://<host>, no trailing slash
	secret   string
	project  int64
	repo     string // owner/name
	token    string
	logGroup string
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

// loadInputs reads and checks the test's variables, reporting every
// problem at once, each naming its variable. It never reads GITHUB_TOKEN,
// which might be the deployment's own.
func loadInputs(getenv func(string) string) (inputs, error) {
	var problems []string
	in := inputs{
		secret:   getenv("SENTRY_CLIENT_SECRET"),
		repo:     getenv("POSTDEPLOY_GITHUB_REPO"),
		token:    getenv("POSTDEPLOY_GITHUB_TOKEN"),
		logGroup: getenv("POSTDEPLOY_LOG_GROUP"),
	}
	if in.logGroup == "" {
		in.logGroup = defaultLogGroup
	}

	switch base, err := parseBaseURL(getenv("POSTDEPLOY_BASE_URL")); {
	case err != nil:
		problems = append(problems, "POSTDEPLOY_BASE_URL "+err.Error())
	default:
		in.baseURL = base
	}

	if in.secret == "" {
		problems = append(problems, "SENTRY_CLIENT_SECRET must be set to the deployment's Sentry client secret")
	}

	switch v := getenv("POSTDEPLOY_SENTRY_PROJECT_ID"); {
	case v == "":
		problems = append(problems, "POSTDEPLOY_SENTRY_PROJECT_ID must be set to the Sentry project id the deployment maps to the repo")
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			problems = append(problems, fmt.Sprintf("POSTDEPLOY_SENTRY_PROJECT_ID must be a positive integer, got %q", v))
		}
		in.project = id
	}

	switch {
	case in.repo == "":
		problems = append(problems, "POSTDEPLOY_GITHUB_REPO must be set to the throwaway repo, as owner/name")
	case !repoPattern.MatchString(in.repo) || strings.HasSuffix(in.repo, "/.") || strings.HasSuffix(in.repo, "/.."):
		problems = append(problems, fmt.Sprintf("POSTDEPLOY_GITHUB_REPO must be owner/name, got %q", in.repo))
	case strings.EqualFold(in.repo, upstreamRepo):
		problems = append(problems, "POSTDEPLOY_GITHUB_REPO must be a throwaway repo you own, not "+upstreamRepo)
	}

	switch {
	case in.token == "":
		problems = append(problems, "POSTDEPLOY_GITHUB_TOKEN must be set to a fine-grained token for the repo (GITHUB_TOKEN is never read)")
	case strings.ContainsFunc(in.token, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		problems = append(problems, "POSTDEPLOY_GITHUB_TOKEN must not contain whitespace or control characters")
	}

	if len(problems) > 0 {
		return inputs{}, errors.New(strings.Join(problems, "\n"))
	}
	return in, nil
}

// parseBaseURL accepts an absolute https URL with no path beyond "/" and
// returns it without the trailing slash. http is refused: the ALB answers
// it with a 301, which turns the POST into a GET.
func parseBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("must be set to the deployment's base URL (tofu -chdir=infra output -raw base_url)")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("is not a URL: %v", err)
	case u.Scheme != "https":
		return "", fmt.Errorf("must be an https URL, got %q", raw)
	case u.Host == "" || u.Opaque != "":
		return "", fmt.Errorf("must have a host, got %q", raw)
	case u.User != nil:
		return "", fmt.Errorf("must not carry user info, got %q", raw)
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("must have no path, got %q", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("must have no query or fragment, got %q", raw)
	}
	return "https://" + u.Host, nil
}

// parseLsRemote parses git ls-remote's "<sha>\t<ref>" lines into ref → sha.
func parseLsRemote(out string) (map[string]string, error) {
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok || sha == "" || ref == "" {
			return nil, fmt.Errorf("unexpected ls-remote line %q", line)
		}
		refs[ref] = sha
	}
	return refs, nil
}

// unexpectedRefs returns, sorted, the refs the reset may not find: all
// but HEAD, the default branch, heinzel/* branches and refs/pull/*. Tags
// and other branches would reach heinzel's clone --bare and could hold
// the fix.
func unexpectedRefs(refs map[string]string, defaultBranch string) []string {
	var bad []string
	for ref := range refs {
		switch {
		case ref == "HEAD", ref == "refs/heads/"+defaultBranch,
			strings.HasPrefix(ref, heinzelPrefix), strings.HasPrefix(ref, "refs/pull/"):
		default:
			bad = append(bad, ref)
		}
	}
	slices.Sort(bad)
	return bad
}

// heinzelBranches returns the heinzel/* branches, sorted.
func heinzelBranches(refs map[string]string) []string {
	var branches []string
	for ref := range refs {
		if strings.HasPrefix(ref, heinzelPrefix) {
			branches = append(branches, ref)
		}
	}
	slices.Sort(branches)
	return branches
}

// hasHeads reports whether any branch exists.
func hasHeads(refs map[string]string) bool {
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			return true
		}
	}
	return false
}

// checkResetRefs requires exactly HEAD and the default branch, both at
// parent, plus any refs/pull/*.
func checkResetRefs(refs map[string]string, defaultBranch, parent string) error {
	var problems []string
	for _, ref := range []string{"HEAD", "refs/heads/" + defaultBranch} {
		if refs[ref] != parent {
			problems = append(problems, fmt.Sprintf("%s is at %q, want %s", ref, refs[ref], parent))
		}
	}
	var extra []string
	for ref := range refs {
		if ref != "HEAD" && ref != "refs/heads/"+defaultBranch && !strings.HasPrefix(ref, "refs/pull/") {
			extra = append(extra, ref)
		}
	}
	slices.Sort(extra)
	for _, ref := range extra {
		problems = append(problems, "unexpected ref "+ref)
	}
	if len(problems) > 0 {
		return errors.New("after the reset: " + strings.Join(problems, "; "))
	}
	return nil
}

// basicAuthHeader is the git http.extraHeader that authenticates as token.
func basicAuthHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// redact replaces token and its git Basic credential in s.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return strings.NewReplacer(basic, "[redacted]", token, "[redacted]").Replace(s)
}

// retryable reports whether a GitHub API answer is worth retrying: a 5xx,
// a 429 or a rate-limit 403.
func retryable(status int, header http.Header, body []byte) bool {
	switch {
	case status >= 500, status == http.StatusTooManyRequests:
		return true
	case status == http.StatusForbidden:
		return header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(string(body)), "rate limit")
	}
	return false
}

// waitDeadline is the earlier of posted + maxWait and the test's deadline
// less deadlineMargin.
func waitDeadline(posted, testDeadline time.Time, hasDeadline bool) time.Time {
	d := posted.Add(maxWait)
	if hasDeadline && testDeadline.Add(-deadlineMargin).Before(d) {
		return testDeadline.Add(-deadlineMargin)
	}
	return d
}

type repoRef struct {
	FullName string `json:"full_name"`
}

// pullRequest is what the test reads of a GitHub PR.
type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	State   string `json:"state"`
	Head    struct {
		Ref  string   `json:"ref"`
		SHA  string   `json:"sha"`
		Repo *repoRef `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// prProblems lists how pr differs from the draft PR heinzel should open
// from repo against defaultBranch.
func prProblems(pr pullRequest, repo, defaultBranch string) []string {
	var problems []string
	if !pr.Draft {
		problems = append(problems, "draft = false, want true")
	}
	if pr.State != "open" {
		problems = append(problems, fmt.Sprintf("state = %q, want \"open\"", pr.State))
	}
	if pr.Base.Ref != defaultBranch {
		problems = append(problems, fmt.Sprintf("base.ref = %q, want %q", pr.Base.Ref, defaultBranch))
	}
	if pr.Head.Repo == nil || pr.Head.Repo.FullName != repo {
		got := "<none>"
		if pr.Head.Repo != nil {
			got = pr.Head.Repo.FullName
		}
		problems = append(problems, fmt.Sprintf("head.repo.full_name = %q, want %q", got, repo))
	}
	return problems
}

// timeoutMessage is the failure when no PR appeared in time.
func timeoutMessage(repo, branch, issueID, logGroup string, project int64, waited time.Duration) string {
	return fmt.Sprintf("no PR from %s appeared in %s within %v of posting the alert for issue %s.\n"+
		"Read heinzel's logs for the run: aws logs tail %s --since 1h --filter-pattern '\"%s\"'\n"+
		"If the run ended as Skipped \"unmapped\", the deployment's sentry_github_repos doesn't map project %d to %s.",
		branch, repo, formatDuration(waited), issueID, logGroup, issueID, project, repo)
}

// formatDuration prints whole minutes as "45m" rather than "45m0s".
func formatDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}
