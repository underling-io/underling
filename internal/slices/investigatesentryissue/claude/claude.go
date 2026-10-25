// Package claude runs headless Claude Code in a sandboxed child process: it
// builds the command line, environment and sandbox settings for a run in a
// workspace, runs the child and reads its stream-json output.
package claude

import (
	"encoding/json"
	"path/filepath"
	"slices"

	"heinzel/internal/slices/investigatesentryissue/workspace"
)

// openAIHost is the only host sandboxed commands may reach: the Codex CLI
// calls the OpenAI API with OPENAI_API_KEY.
const openAIHost = "api.openai.com"

// Config is what a claude run needs from heinzel's settings.
type Config struct {
	Bin          string
	PluginDirs   []string
	Path         string // PATH handed to the child
	AnthropicKey string
	OpenAIKey    string
	// ParentHome is heinzel's own HOME; empty if unset. Neither the
	// child's file tools nor its sandboxed commands may read it.
	ParentHome string
	// DenyRead are more paths the child may not read, such as heinzel's
	// data dir.
	DenyRead []string
	// Readable are the paths under denied ones that sandboxed commands
	// still need to read: the plugin dirs and the claude binary's dir.
	Readable []string
}

// Cmd is one invocation of the claude CLI.
type Cmd struct {
	bin  string
	args []string
	env  []string
	dir  string
	// stdin is the prompt. -p reads it from stdin when argv has none, so
	// it never shows in the process list or meets argv size limits.
	stdin string
	// pluginDirs are the --plugin-dir values the session must report as
	// loaded.
	pluginDirs []string
}

// Command builds the headless claude invocation for ws. The child gets
// only PATH, its own HOME and config dir, and the two model credentials;
// nothing else from heinzel's environment.
func Command(cfg Config, ws *workspace.Workspace, prompt string) (Cmd, error) {
	settings, err := sandboxSettings(cfg, ws)
	if err != nil {
		return Cmd{}, err
	}
	args := []string{
		"--bare", "-p",
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
		stdin:      prompt,
		pluginDirs: cfg.PluginDirs,
		dir:        ws.Clone,
		env: []string{
			"PATH=" + cfg.Path,
			"HOME=" + ws.Home,
			"CLAUDE_CONFIG_DIR=" + ws.ClaudeConfig,
			"ANTHROPIC_API_KEY=" + cfg.AnthropicKey,
			"OPENAI_API_KEY=" + cfg.OpenAIKey,
		},
	}, nil
}

// settings is the subset of Claude Code's settings.json heinzel generates.
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

// sandboxSettings returns the settings JSON for a run in ws. Shell commands
// run in Claude Code's sandbox, which refuses to start unsandboxed, never
// retries outside it, may write only to the clone and the child's HOME
// (plus the sandbox's own temp dir) and may reach only the OpenAI API. File
// edits are auto-approved inside the working directory (the clone) only;
// anything that would need a prompt is denied, since nobody can answer one.
//
// Reads are denied for heinzel's HOME, the configured denyRead paths and
// the whole work dir. Sandboxed commands get the clone, the child's HOME and
// Claude config dir (shell snapshots, plugin data), the plugin dirs and the
// claude binary's dir re-opened (a narrower
// allowRead wins over a broader denyRead). Read deny rules can't be carved
// out, so for the file tools the work dir is denied piecemeal: everything
// in it but this run's clone, HOME and Claude config dir.
func sandboxSettings(cfg Config, ws *workspace.Workspace) ([]byte, error) {
	denied := WithResolved(append([]string{cfg.ParentHome}, cfg.DenyRead...))

	var s settings
	s.Permissions.DefaultMode = "acceptEdits"
	s.Permissions.DisableBypassPermissionsMode = "disable"
	s.Permissions.Deny = []string{"Read(//proc/**)"}
	for _, p := range slices.Concat(denied, ws.Siblings, []string{ws.Mirror, ws.GitHome}) {
		s.Permissions.Deny = append(s.Permissions.Deny, readDenyRule(p))
	}
	s.Sandbox.Enabled = true
	s.Sandbox.FailIfUnavailable = true
	s.Sandbox.AutoAllowBashIfSandboxed = true
	s.Sandbox.AllowUnsandboxedCommands = false
	s.Sandbox.Filesystem.AllowWrite = []string{ws.Clone, ws.Home}
	s.Sandbox.Filesystem.DenyRead = append(denied, ws.WorkDir)
	s.Sandbox.Filesystem.AllowRead = slices.Concat([]string{ws.Clone, ws.Home, ws.ClaudeConfig}, cfg.Readable)
	s.Sandbox.Network.AllowedDomains = []string{openAIHost}
	return json.Marshal(s)
}

// readDenyRule is the permission rule that keeps Claude's file tools out of
// dir, an absolute path: rules mark absolute paths with a leading "//".
func readDenyRule(dir string) string {
	return "Read(/" + filepath.ToSlash(dir) + "/**)"
}
