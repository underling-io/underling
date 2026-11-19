package workspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	gitNetworkTimeout = 10 * time.Minute // clone, fetch, push and bundle
	gitLocalTimeout   = time.Minute      // everything else
	// gitWaitDelay bounds how long a killed git's output is still read,
	// e.g. while a lingering git-remote-https holds the pipes.
	gitWaitDelay     = 10 * time.Second
	gitLowSpeedLimit = "1000" // bytes per second …
	gitLowSpeedTime  = "60"   // … for this many seconds aborts a transfer
	gitOutputMax     = 2 << 10
)

// gitRunner runs git with a fixed environment of its own. Only the
// commands that talk to GitHub get the token.
type gitRunner struct {
	path, home, token string
	networkTimeout    time.Duration
	localTimeout      time.Duration
	waitDelay         time.Duration
}

func newGitRunner(path, home, token string) gitRunner {
	return gitRunner{
		path: path, home: home, token: token,
		networkTimeout: gitNetworkTimeout,
		localTimeout:   gitLocalTimeout,
		waitDelay:      gitWaitDelay,
	}
}

// network runs a git command that talks to GitHub, with the token.
func (g gitRunner) network(ctx context.Context, dir string, args ...string) (string, error) {
	return g.run(ctx, dir, true, args...)
}

// local runs a git command that stays on this machine, without the token.
func (g gitRunner) local(ctx context.Context, dir string, args ...string) (string, error) {
	return g.run(ctx, dir, false, args...)
}

// run runs git in dir and returns its trimmed output. Errors carry git's
// output, the token redacted.
func (g gitRunner) run(ctx context.Context, dir string, auth bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout(args))
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env(auth)
	cmd.WaitDelay = g.waitDelay
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err == nil {
		return text, nil
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	text = text[max(len(text)-gitOutputMax, 0):]
	if g.token != "" {
		text = strings.ReplaceAll(text, g.token, "[redacted]")
	}
	return "", fmt.Errorf("git %s: %w: %s", subcommand(args), err, text)
}

func (g gitRunner) timeout(args []string) time.Duration {
	switch subcommand(args) {
	case "clone", "fetch", "push", "bundle":
		return g.networkTimeout
	}
	return g.localTimeout
}

// subcommand returns the git subcommand in args, past any -c or -C option.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// env is git's whole environment: no system or global config, and its own
// HOME. With auth, the token goes in an HTTP header through GIT_CONFIG_*,
// never argv, a remote URL or a config file, and slow transfers abort.
func (g gitRunner) env(auth bool) []string {
	env := []string{
		"PATH=" + g.path,
		"HOME=" + g.home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	if !auth {
		return env
	}
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))
	config := [][2]string{
		{"http.extraHeader", "Authorization: Basic " + credentials},
		{"http.lowSpeedLimit", gitLowSpeedLimit},
		{"http.lowSpeedTime", gitLowSpeedTime},
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}
