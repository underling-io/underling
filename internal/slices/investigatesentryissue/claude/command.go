// Package claude runs headless Claude Code in a sandboxed child process:
// it builds the child's argv, environment and sandbox settings, runs it,
// and reads its stream-json output.
package claude

import (
	"encoding/json"
	"path/filepath"
	"slices"
)

// openAIHost is the only host sandboxed commands may reach: the Codex CLI
// calls the OpenAI API. In Bedrock mode it calls bedrockHost instead.
const openAIHost = "api.openai.com"

// bedrockHost is the Bedrock host Codex's amazon-bedrock provider calls in
// region.
func bedrockHost(region string) string { return "bedrock-mantle." + region + ".api.aws" }

// Config is what a run needs from heinzel's settings.
type Config struct {
	Bin        string
	PluginDirs []string
	Path       string // the child's PATH
	Anthropic  Credential
	OpenAIKey  string
	// ParentHome is heinzel's own HOME, or "". The child may not read it.
	ParentHome string
	// DenyRead are more paths the child may not read, such as heinzel's
	// data dir.
	DenyRead []string
	// Readable are paths under denied ones that sandboxed commands still
	// need: the plugin dirs and the claude binary's dir.
	Readable []string
	// Bedrock is set in Bedrock mode: the child logs in to Amazon Bedrock
	// with AWS credentials, and Anthropic and OpenAIKey are unused.
	Bedrock *Bedrock
}

// Bedrock is how Claude and Codex reach Amazon Bedrock.
type Bedrock struct {
	Region         string // Claude's
	Model          string
	SmallFastModel string
	CodexRegion    string
	Credentials    AWSCredentials // this run's
}

// AWSCredentials are AWS credentials as env vars carry them.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // "" for long-term credentials
}

// Credential is how the child logs in to Anthropic: an API key, or a
// subscription OAuth token from `claude setup-token`.
type Credential struct {
	OAuth bool
	Value string
}

// envVar returns the variable the child reads the credential from.
func (c Credential) envVar() string {
	if c.OAuth {
		return "CLAUDE_CODE_OAUTH_TOKEN"
	}
	return "ANTHROPIC_API_KEY"
}

// Dirs are the paths of one run, symlinks resolved.
type Dirs struct {
	WorkDir      string
	Siblings     []string // the work dir's other entries
	Clone        string   // where Claude works
	Home         string   // the child's HOME
	ClaudeConfig string   // the child's CLAUDE_CONFIG_DIR
	Mirror       string   // heinzel's mirror, which holds the token's pushes
	GitHome      string   // the HOME of heinzel's own git
}

// Cmd is one invocation of the claude CLI.
type Cmd struct {
	bin  string
	args []string
	env  []string
	dir  string
	// stdin is the prompt: -p reads it there when argv has none, so it
	// never shows in the process list or meets argv limits.
	stdin string
	// pluginDirs must each show up as loaded in the session's init event.
	pluginDirs []string
}

// Command builds the headless claude invocation for a run in dirs. The
// child gets only PATH, its own HOME and config dir, and the two model
// credentials from heinzel's environment, or in Bedrock mode its Bedrock
// settings, with the model aliases pinned to them, and AWS credentials.
//
// --bare never reads an OAuth token, so a run on one goes without it and
// skips MCP servers with --strict-mcp-config instead; the fresh config dir
// and --setting-sources "" keep installed plugins and settings out.
func Command(cfg Config, dirs Dirs, prompt string) (Cmd, error) {
	settings, err := json.Marshal(sandboxSettings(cfg, dirs))
	if err != nil {
		return Cmd{}, err
	}
	minimal := "--bare"
	if cfg.Anthropic.OAuth {
		minimal = "--strict-mcp-config"
	}
	args := []string{
		minimal, "-p",
		"--verbose", "--output-format", "stream-json",
		"--no-session-persistence",
		"--setting-sources", "",
		"--settings", string(settings),
		"--permission-mode", "acceptEdits",
		"--permission-prompts", "none",
	}
	for _, dir := range cfg.PluginDirs {
		args = append(args, "--plugin-dir", dir)
	}
	return Cmd{
		bin:        cfg.Bin,
		args:       args,
		dir:        dirs.Clone,
		env:        childEnv(cfg, dirs),
		stdin:      prompt,
		pluginDirs: cfg.PluginDirs,
	}, nil
}

// childEnv is the child's whole environment.
func childEnv(cfg Config, dirs Dirs) []string {
	env := []string{
		"PATH=" + cfg.Path,
		"HOME=" + dirs.Home,
		"CLAUDE_CONFIG_DIR=" + dirs.ClaudeConfig,
	}
	b := cfg.Bedrock
	if b == nil {
		return append(env,
			cfg.Anthropic.envVar()+"="+cfg.Anthropic.Value,
			"OPENAI_API_KEY="+cfg.OpenAIKey,
		)
	}
	env = append(env,
		"CLAUDE_CODE_USE_BEDROCK=1",
		"AWS_REGION="+b.Region,
		"ANTHROPIC_MODEL="+b.Model,
		"ANTHROPIC_SMALL_FAST_MODEL="+b.SmallFastModel,
		// Pin the aliases too: a subagent or skill with `model: sonnet`
		// (or opus, haiku) would otherwise land on Claude Code's default
		// Bedrock model, which the IAM role may not grant.
		"ANTHROPIC_DEFAULT_OPUS_MODEL="+b.Model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL="+b.Model,
		"ANTHROPIC_DEFAULT_FABLE_MODEL="+b.Model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL="+b.SmallFastModel,
		"AWS_ACCESS_KEY_ID="+b.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+b.Credentials.SecretAccessKey,
	)
	if b.Credentials.SessionToken != "" {
		env = append(env, "AWS_SESSION_TOKEN="+b.Credentials.SessionToken)
	}
	return env
}

// settings is the part of Claude Code's settings.json heinzel generates.
type settings struct {
	Permissions struct {
		DefaultMode                  string   `json:"defaultMode"`
		DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
		Deny                         []string `json:"deny"`
	} `json:"permissions"`
	Sandbox struct {
		Enabled                  bool `json:"enabled"`
		FailIfUnavailable        bool `json:"failIfUnavailable"`
		AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
		AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
		Filesystem               struct {
			AllowWrite []string `json:"allowWrite"`
			DenyRead   []string `json:"denyRead"`
			AllowRead  []string `json:"allowRead"`
		} `json:"filesystem"`
		Network struct {
			AllowedDomains []string `json:"allowedDomains"`
		} `json:"network"`
	} `json:"sandbox"`
}

// sandboxSettings returns the settings for a run in dirs.
//
// Shell commands run in Claude Code's sandbox, which refuses to start
// unsandboxed and never retries outside it. They may write only to the
// clone and the child's HOME, and reach only the API Codex calls: OpenAI's,
// or in Bedrock mode Bedrock's in the Codex region. Reads of
// heinzel's HOME, the deny-read paths and the whole work dir are denied;
// a narrower allowRead re-opens the clone, the child's HOME and config dir,
// the plugin dirs and the claude binary's dir.
//
// Claude's file tools aren't sandboxed, so Read deny rules keep them out
// of the same places. Those rules can't be carved out, so the work dir is
// denied piecemeal: every other entry in it, plus the mirror and git-home.
func sandboxSettings(cfg Config, dirs Dirs) settings {
	denied := WithResolved(append([]string{cfg.ParentHome}, cfg.DenyRead...))

	var s settings
	s.Permissions.DefaultMode = "acceptEdits"
	s.Permissions.DisableBypassPermissionsMode = "disable"
	s.Permissions.Deny = []string{"Read(//proc/**)"}
	for _, p := range slices.Concat(denied, dirs.Siblings, []string{dirs.Mirror, dirs.GitHome}) {
		s.Permissions.Deny = append(s.Permissions.Deny, readDenyRule(p))
	}
	s.Sandbox.Enabled = true
	s.Sandbox.FailIfUnavailable = true
	s.Sandbox.AutoAllowBashIfSandboxed = true
	s.Sandbox.AllowUnsandboxedCommands = false
	s.Sandbox.Filesystem.AllowWrite = []string{dirs.Clone, dirs.Home}
	s.Sandbox.Filesystem.DenyRead = append(denied, dirs.WorkDir)
	s.Sandbox.Filesystem.AllowRead = slices.Concat([]string{dirs.Clone, dirs.Home, dirs.ClaudeConfig}, cfg.Readable)
	s.Sandbox.Network.AllowedDomains = []string{openAIHost}
	if cfg.Bedrock != nil {
		s.Sandbox.Network.AllowedDomains = []string{bedrockHost(cfg.Bedrock.CodexRegion)}
	}
	return s
}

// readDenyRule keeps Claude's file tools out of dir, an absolute path;
// permission rules mark absolute paths with a leading "//".
func readDenyRule(dir string) string {
	return "Read(/" + filepath.ToSlash(dir) + "/**)"
}
