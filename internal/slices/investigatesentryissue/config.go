package investigatesentryissue

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"underling/internal/slices/investigatesentryissue/claude"
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
	apiURLEnvVar     = "GITHUB_API_URL"
	cloneURLEnvVar   = "GITHUB_CLONE_URL"

	useBedrockEnvVar     = "CLAUDE_CODE_USE_BEDROCK"
	awsRegionEnvVar      = "AWS_REGION"
	modelEnvVar          = "ANTHROPIC_MODEL"
	smallFastModelEnvVar = "ANTHROPIC_SMALL_FAST_MODEL"
	codexModelEnvVar     = "CODEX_BEDROCK_MODEL"
	codexRegionEnvVar    = "CODEX_BEDROCK_REGION"
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
	path        string // the child's PATH: underling's own
	parentHome  string // underling's HOME, or ""; the child may not read it
	denyRead    []string
	// readable are the paths under denied ones that sandboxed commands
	// still need: the plugin dirs and the claude binary's dir. Set by
	// resolveReadPaths.
	readable  []string
	apiURL    string // the GitHub REST API base
	cloneBase string // repos are cloned from <cloneBase><owner>/<repo>.git
	// bedrock is set in Bedrock mode, where Claude and Codex use Amazon
	// Bedrock with underling's AWS credentials instead of the two keys.
	bedrock *bedrockConfig
}

// bedrockConfig is Bedrock mode's settings.
type bedrockConfig struct {
	region         string // Claude's AWS region
	model          string // Claude's model or inference-profile id
	smallFastModel string
	codexModel     string
	codexRegion    string
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
	check := func(variable string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", variable, err))
		}
	}
	var err error
	cfg.repos, err = parseRepos(getenv(reposEnvVar))
	check(reposEnvVar, err)
	required := []struct{ name, value string }{{githubTokenVar, cfg.githubToken}}
	switch getenv(useBedrockEnvVar) {
	case "":
		cfg.anthropic, err = parseCredential(getenv(anthropicKeyVar), getenv(oauthTokenVar))
		if err != nil {
			errs = append(errs, err)
		}
		required = append(required, struct{ name, value string }{openAIKeyVar, cfg.openAIKey})
	case "1":
		var bedrockErrs []error
		cfg.bedrock, bedrockErrs = parseBedrock(getenv)
		errs = append(errs, bedrockErrs...)
	default:
		check(useBedrockEnvVar, errors.New(`must be "1" or unset`))
	}
	for _, v := range required {
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
	cfg.apiURL, err = parseAPIURL(getenv(apiURLEnvVar))
	check(apiURLEnvVar, err)
	cfg.cloneBase, err = parseCloneBase(getenv(cloneURLEnvVar))
	check(cloneURLEnvVar, err)
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

var (
	// awsRegion is an AWS region's shape, such as eu-central-1; it ends up
	// in a hostname.
	awsRegion = regexp.MustCompile(`^[a-z]+(-[a-z]+)+-[0-9]+$`)
	// modelID is a Bedrock model or inference-profile id; it ends up in
	// TOML and a request path.
	modelID = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
)

// parseBedrock reads Bedrock mode's settings. Neither Anthropic nor OpenAI
// keys may be set, so the child never logs in any other way.
func parseBedrock(getenv func(string) string) (*bedrockConfig, []error) {
	var errs []error
	fail := func(variable, problem string) {
		errs = append(errs, fmt.Errorf("%s: %s", variable, problem))
	}
	// read returns variable's value, or def for "". A required variable
	// has no default.
	read := func(variable string, required bool, def string, valid *regexp.Regexp, shape string) string {
		value := getenv(variable)
		switch {
		case value == "" && required:
			fail(variable, "must be set in Bedrock mode")
		case value == "":
			return def
		case !valid.MatchString(value):
			fail(variable, fmt.Sprintf("%q is not %s", value, shape))
		}
		return value
	}
	const regionShape, modelShape = "an AWS region such as eu-central-1", "a Bedrock model id of [A-Za-z0-9._:/-]"
	b := &bedrockConfig{}
	b.region = read(awsRegionEnvVar, true, "", awsRegion, regionShape)
	b.model = read(modelEnvVar, true, "", modelID, modelShape)
	b.smallFastModel = read(smallFastModelEnvVar, false, b.model, modelID, modelShape)
	b.codexModel = read(codexModelEnvVar, true, "", modelID, modelShape)
	b.codexRegion = read(codexRegionEnvVar, false, b.region, awsRegion, regionShape)
	for _, key := range []string{anthropicKeyVar, oauthTokenVar, openAIKeyVar} {
		if getenv(key) != "" {
			fail(key, "must not be set in Bedrock mode")
		}
	}
	return b, errs
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

// parseAPIURL parses an absolute http(s) URL without credentials, query or
// fragment (paths are appended to it), or returns the GitHub API for "".
func parseAPIURL(value string) (string, error) {
	if value == "" {
		return "https://api.github.com", nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not an absolute http(s) URL", value)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q has credentials, a query or a fragment", value)
	}
	return value, nil
}

// parseCloneBase checks a clone base, a URL or a local dir, ends in "/",
// or returns GitHub's for "".
func parseCloneBase(value string) (string, error) {
	if value == "" {
		return "https://github.com/", nil
	}
	if !strings.HasSuffix(value, "/") {
		return "", fmt.Errorf("%q does not end in /", value)
	}
	return value, nil
}
