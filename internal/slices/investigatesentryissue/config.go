package investigatesentryissue

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"heinzel/internal/slices/investigatesentryissue/claude"
)

const (
	reposEnvVar      = "SENTRY_GITHUB_REPOS"
	githubTokenVar   = "GITHUB_TOKEN"
	anthropicKeyVar  = "ANTHROPIC_API_KEY"
	oauthTokenVar    = "CLAUDE_CODE_OAUTH_TOKEN"
	openAIKeyVar     = "OPENAI_API_KEY"
	pluginDirsEnvVar = "CLAUDE_PLUGIN_DIRS"
	timeoutEnvVar    = "INVESTIGATION_TIMEOUT"
	maxAgeEnvVar     = "INVESTIGATION_MAX_AGE"
	claudeBinEnvVar  = "CLAUDE_BIN"
	workDirEnvVar    = "INVESTIGATION_WORK_DIR"
	denyReadEnvVar   = "INVESTIGATION_DENY_READ"
)

// repo is a GitHub repository.
type repo struct {
	owner, name string
}

func (r repo) String() string { return r.owner + "/" + r.name }

// config is the slice's settings.
type config struct {
	repos       map[int64]repo // by Sentry project id
	githubToken string
	anthropic   claude.Credential
	openAIKey   string
	pluginDirs  []string
	timeout     time.Duration
	maxAge      time.Duration
	claudeBin   string
	workDir     string // "" until New picks the default
	path        string // the child's PATH: heinzel's own
	parentHome  string // heinzel's HOME, or ""; the child may not read it
	denyRead    []string
	// readable are the paths under denied ones that sandboxed commands
	// still need: the plugin dirs and the claude binary's dir. Set by
	// resolveReadPaths.
	readable []string
}

// loadConfig reads the settings through getenv. It reports every invalid
// value, each naming its variable.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		githubToken: getenv(githubTokenVar),
		openAIKey:   getenv(openAIKeyVar),
		claudeBin:   getenv(claudeBinEnvVar),
		workDir:     getenv(workDirEnvVar),
		path:        getenv("PATH"),
	}
	if cfg.claudeBin == "" {
		cfg.claudeBin = "claude"
	}
	if home := getenv("HOME"); home != "" {
		cfg.parentHome = filepath.Clean(home)
	}

	var errs []error
	var err error
	check := func(variable string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", variable, err))
		}
	}
	cfg.repos, err = parseRepos(getenv(reposEnvVar))
	check(reposEnvVar, err)
	cfg.anthropic, err = parseCredential(getenv(anthropicKeyVar), getenv(oauthTokenVar))
	if err != nil {
		errs = append(errs, err)
	}
	for _, v := range []struct{ name, value string }{{githubTokenVar, cfg.githubToken}, {openAIKeyVar, cfg.openAIKey}} {
		if v.value == "" {
			check(v.name, errors.New("must be set"))
		}
	}
	cfg.pluginDirs, err = parseList(getenv(pluginDirsEnvVar))
	check(pluginDirsEnvVar, err)
	cfg.timeout, err = parseDuration(getenv(timeoutEnvVar), 30*time.Minute)
	check(timeoutEnvVar, err)
	cfg.maxAge, err = parseDuration(getenv(maxAgeEnvVar), time.Hour)
	check(maxAgeEnvVar, err)
	cfg.denyRead, err = parsePaths(getenv(denyReadEnvVar))
	check(denyReadEnvVar, err)
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// parseCredential returns the Anthropic credential: an API key or an OAuth
// token, exactly one of them.
func parseCredential(apiKey, oauthToken string) (claude.Credential, error) {
	switch {
	case apiKey != "" && oauthToken != "":
		return claude.Credential{}, fmt.Errorf("%s and %s: set only one", anthropicKeyVar, oauthTokenVar)
	case apiKey != "":
		return claude.Credential{Value: apiKey}, nil
	case oauthToken != "":
		return claude.Credential{OAuth: true, Value: oauthToken}, nil
	}
	return claude.Credential{}, fmt.Errorf("%s or %s: must be set", anthropicKeyVar, oauthTokenVar)
}

var repoNamePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseRepos parses comma-separated <projectId>=<owner>/<repo> pairs.
func parseRepos(value string) (map[int64]repo, error) {
	pairs, err := parseList(value)
	if err != nil {
		return nil, err
	}
	repos := map[int64]repo{}
	for _, pair := range pairs {
		idText, fullName, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not <projectId>=<owner>/<repo>", pair)
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("project id %q is not an integer", idText)
		}
		if _, dup := repos[id]; dup {
			return nil, fmt.Errorf("project id %d is mapped twice", id)
		}
		owner, name, ok := strings.Cut(strings.TrimSpace(fullName), "/")
		if !ok || !validNamePart(owner) || !validNamePart(name) {
			return nil, fmt.Errorf("%q is not a GitHub <owner>/<repo>", fullName)
		}
		repos[id] = repo{owner: owner, name: name}
	}
	return repos, nil
}

func validNamePart(s string) bool {
	return repoNamePart.MatchString(s) && s != "." && s != ".."
}

// parseList splits a comma-separated list and trims each entry. An empty
// list or entry is an error.
func parseList(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("must be set")
	}
	var items []string
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%q has an empty entry", value)
		}
		items = append(items, item)
	}
	return items, nil
}

// parsePaths parses an optional comma-separated list of absolute paths.
func parsePaths(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	items, err := parseList(value)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(items))
	for i, item := range items {
		if !filepath.IsAbs(item) {
			return nil, fmt.Errorf("%q is not an absolute path", item)
		}
		paths[i] = filepath.Clean(item)
	}
	return paths, nil
}

// parseDuration parses a positive Go duration, or returns def for "".
func parseDuration(value string, def time.Duration) (time.Duration, error) {
	if value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s is not positive", value)
	}
	return d, nil
}
