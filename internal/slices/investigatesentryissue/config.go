package investigatesentryissue

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"heinzel/internal/slices/investigatesentryissue/claude"
)

const (
	reposEnvVar      = "SENTRY_GITHUB_REPOS"
	githubTokenVar   = "GITHUB_TOKEN"
	anthropicKeyVar  = "ANTHROPIC_API_KEY"
	openAIKeyVar     = "OPENAI_API_KEY"
	pluginDirsEnvVar = "CLAUDE_PLUGIN_DIRS"
	timeoutEnvVar    = "INVESTIGATION_TIMEOUT"
	maxAgeEnvVar     = "INVESTIGATION_MAX_AGE"
	claudeBinEnvVar  = "CLAUDE_BIN"
	workDirEnvVar    = "INVESTIGATION_WORK_DIR"
	denyReadEnvVar   = "INVESTIGATION_DENY_READ"

	defaultTimeout   = 30 * time.Minute
	defaultMaxAge    = time.Hour
	defaultClaudeBin = "claude"
)

// repoPart is one segment of a GitHub owner/repo name.
var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// repo is a GitHub repository.
type repo struct {
	owner, name string
}

func (r repo) String() string { return r.owner + "/" + r.name }

// config is the slice's settings, loaded from the environment.
type config struct {
	repos        map[int64]repo // by Sentry project id
	githubToken  string
	anthropicKey string
	openAIKey    string
	pluginDirs   []string
	timeout      time.Duration
	maxAge       time.Duration
	claudeBin    string
	workDir      string // empty means workspace.DefaultDir()
	path         string // PATH handed to the claude child
	// parentHome is heinzel's own HOME; empty if unset. Neither the
	// child's file tools nor its sandboxed commands may read it.
	parentHome string
	// denyRead are more paths the child may not read, such as heinzel's
	// data dir.
	denyRead []string
	// readable are the paths under denied ones that sandboxed commands
	// still need to read: the plugin dirs and the claude binary's dir.
	// resolveReadPaths sets it.
	readable []string
}

// loadConfig reads the slice's settings through getenv. Any invalid value is
// an error naming its variable.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		githubToken:  getenv(githubTokenVar),
		anthropicKey: getenv(anthropicKeyVar),
		openAIKey:    getenv(openAIKeyVar),
		claudeBin:    getenv(claudeBinEnvVar),
		workDir:      getenv(workDirEnvVar),
		path:         getenv("PATH"),
	}
	// HOME is what os.UserHomeDir reads on Unix; reading it through getenv
	// keeps the config testable.
	if home := getenv("HOME"); home != "" {
		cfg.parentHome = filepath.Clean(home)
	}
	var errs []error
	var err error
	if cfg.repos, err = parseRepos(getenv(reposEnvVar)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", reposEnvVar, err))
	}
	for name, value := range map[string]string{githubTokenVar: cfg.githubToken, anthropicKeyVar: cfg.anthropicKey, openAIKeyVar: cfg.openAIKey} {
		if value == "" {
			errs = append(errs, errors.New(name+" must be set"))
		}
	}
	if cfg.pluginDirs, err = parseList(getenv(pluginDirsEnvVar)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", pluginDirsEnvVar, err))
	}
	if cfg.timeout, err = parseDuration(getenv(timeoutEnvVar), defaultTimeout); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", timeoutEnvVar, err))
	}
	if cfg.maxAge, err = parseDuration(getenv(maxAgeEnvVar), defaultMaxAge); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", maxAgeEnvVar, err))
	}
	if cfg.denyRead, err = parsePaths(getenv(denyReadEnvVar)); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", denyReadEnvVar, err))
	}
	if cfg.claudeBin == "" {
		cfg.claudeBin = defaultClaudeBin
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// parseRepos parses comma-separated <projectId>=<owner>/<repo> pairs.
func parseRepos(value string) (map[int64]repo, error) {
	pairs, err := parseList(value)
	if err != nil {
		return nil, err
	}
	repos := map[int64]repo{}
	for _, pair := range pairs {
		idText, name, ok := strings.Cut(pair, "=")
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
		owner, repoName, ok := strings.Cut(strings.TrimSpace(name), "/")
		if !ok || !validRepoPart(owner) || !validRepoPart(repoName) {
			return nil, fmt.Errorf("%q is not a GitHub <owner>/<repo>", name)
		}
		repos[id] = repo{owner: owner, name: repoName}
	}
	return repos, nil
}

func validRepoPart(s string) bool {
	return repoPart.MatchString(s) && s != "." && s != ".."
}

// parseList splits a comma-separated list, trimming each entry. It rejects
// an empty list and empty entries.
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
	var paths []string
	for _, item := range items {
		if !filepath.IsAbs(item) {
			return nil, fmt.Errorf("%q is not an absolute path", item)
		}
		paths = append(paths, filepath.Clean(item))
	}
	return paths, nil
}

// resolveReadPaths checks that the paths the child must read stay
// readable despite its read denials, and sets cfg.readable. cfg.workDir
// must be set.
//
// Claude's file tools are denied heinzel's HOME and the denyRead paths
// with Read deny rules, which nothing can carve an exception out of, so the
// work dir and the plugin dirs must lie outside them. Sandboxed commands
// are also denied the work dir; a narrower allowRead re-opens a path inside
// a denied one, but never a denied path inside it. So no denied path may
// lie inside a plugin dir or the claude binary's dir.
func resolveReadPaths(cfg *config) error {
	fileDenied := slices.DeleteFunc(append([]string{cfg.parentHome}, cfg.denyRead...), func(p string) bool { return p == "" })
	for i, p := range fileDenied {
		fileDenied[i] = claude.ResolvePath(p)
	}
	workDir := claude.ResolvePath(cfg.workDir)
	sandboxDenied := append(slices.Clone(fileDenied), workDir)

	if d, ok := claude.FirstContaining(fileDenied, workDir); ok {
		return fmt.Errorf("%s: %s lies under %s, which the child may not read (HOME or %s)", workDirEnvVar, cfg.workDir, d, denyReadEnvVar)
	}
	readable := []string{}
	for _, dir := range cfg.pluginDirs {
		p := claude.ResolvePath(dir)
		for _, d := range sandboxDenied {
			if claude.Within(p, d) || claude.Within(d, p) {
				return fmt.Errorf("%s: plugin dir %s overlaps %s, which the child may not read (HOME, %s or %s); move the plugins elsewhere", pluginDirsEnvVar, dir, d, denyReadEnvVar, workDirEnvVar)
			}
		}
		readable = append(readable, p)
	}
	if bin, ok := claude.LookPath(cfg.claudeBin, cfg.path); ok {
		binDir := filepath.Dir(claude.ResolvePath(bin))
		for _, d := range sandboxDenied {
			if claude.Within(d, binDir) {
				return fmt.Errorf("%s: %s lies in %s, which holds %s, a path the child may not read", claudeBinEnvVar, cfg.claudeBin, binDir, d)
			}
		}
		readable = append(readable, binDir)
	}
	cfg.readable = readable
	return nil
}

// deniedPathDir is a PATH entry under a path the child may not read.
type deniedPathDir struct {
	dir, denied string
}

// deniedPathDirs returns the PATH entries that lie, as given or with
// symlinks resolved, under HOME, a denyRead path or the work dir, and that
// no allowRead entry re-opens. Sandboxed commands can't run tools from
// them. cfg.workDir and cfg.readable must be set.
func deniedPathDirs(cfg config) []deniedPathDir {
	denied := claude.WithResolved(append([]string{cfg.parentHome, cfg.workDir}, cfg.denyRead...))
	var found []deniedPathDir
	for _, dir := range filepath.SplitList(cfg.path) {
		if dir == "" {
			continue
		}
		forms := claude.WithResolved([]string{filepath.Clean(dir)})
		if slices.ContainsFunc(forms, func(f string) bool { _, ok := claude.FirstContaining(cfg.readable, f); return ok }) {
			continue
		}
		for _, f := range forms {
			if d, ok := claude.FirstContaining(denied, f); ok {
				found = append(found, deniedPathDir{dir: dir, denied: d})
				break
			}
		}
	}
	return found
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
