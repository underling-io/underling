package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// waitDelay bounds how long Wait waits for the child's output pipes
	// after it was killed, e.g. when a grandchild still holds them.
	waitDelay = 10 * time.Second
	// drainTimeout bounds how long heinzel reads the child's output after
	// the child exited and its process group was killed.
	drainTimeout = 5 * time.Second
	// stderrTail is how much of the child's stderr a failure reason keeps.
	stderrTail = 2 << 10
)

// TimeoutError is what Run returns when claude ran past its timeout.
type TimeoutError struct {
	After time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("claude did not finish within %v", e.After)
}

// Timeout reports true, as net and os timeout errors do.
func (*TimeoutError) Timeout() bool { return true }

// Run runs cmd until it exits, timeout passes or ctx is cancelled, killing
// it (and its process group) in the latter cases. It returns Claude's
// summary, or an error saying why the run failed: a *TimeoutError after the
// timeout.
func Run(ctx context.Context, cmd Cmd, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, cmd.bin, cmd.args...)
	c.Dir = cmd.dir
	c.Env = cmd.env
	c.WaitDelay = waitDelay
	killProcessGroup(c)

	// heinzel creates the stdio pipes itself and hands the child its ends,
	// so Wait returns as soon as the child exits, even while a tool it left
	// behind still holds them. Goroutines feed and drain them meanwhile.
	var ends [6]*os.File // stdin r/w, stdout r/w, stderr r/w
	for i := 0; i < len(ends); i += 2 {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(ends[:i]...)
			return "", errors.New("create pipe: " + err.Error())
		}
		ends[i], ends[i+1] = r, w
	}
	inR, inW, outR, outW, errR, errW := ends[0], ends[1], ends[2], ends[3], ends[4], ends[5]
	defer closeAll(inW, outR, errR)
	c.Stdin, c.Stdout, c.Stderr = inR, outW, errW
	startErr := c.Start()
	closeAll(inR, outW, errW) // the child has its own copies now
	if startErr != nil {
		return "", errors.New("start claude: " + startErr.Error())
	}

	var (
		out     streamOutput
		readErr error
		stderr  = &tailBuffer{max: stderrTail}
		pipes   sync.WaitGroup
	)
	pipes.Go(func() {
		_, _ = io.WriteString(inW, cmd.stdin)
		_ = inW.Close()
	})
	pipes.Go(func() { out, readErr = readStream(outR) })
	pipes.Go(func() { _, _ = io.Copy(stderr, errR) })

	waitErr := c.Wait()
	// Kill whatever the child left running in its process group, so no
	// tool outlives the run or overlaps the push.
	killGroup(c)
	if !waitOrClose(&pipes, drainTimeout, inW, outR, errR) && errors.Is(readErr, os.ErrClosed) {
		readErr = nil // a process outside the group still held the pipe
	}
	switch {
	case ctx.Err() != nil:
		return "", errors.New("heinzel shut down while claude was running")
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return "", &TimeoutError{After: timeout}
	case waitErr != nil:
		return "", errors.New(joinReason("claude: "+waitErr.Error(), out.summary, stderr.String()))
	case readErr != nil:
		return "", errors.New("read claude output: " + readErr.Error())
	}
	if err := checkInit(out.init, cmd.pluginDirs); err != nil {
		return "", errors.New("claude's session did not start as configured: " + err.Error())
	}
	if !out.ok {
		return "", errors.New(joinReason("claude did not end with a successful result", out.summary, stderr.String()))
	}
	return out.summary, nil
}

// streamEvent is the part of a stream-json event heinzel reads.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// initEvent is the part of the system/init event heinzel checks: the
// session's permission mode and which plugins loaded. Field names follow
// the Agent SDK's SDKSystemMessage, which stream-json emits.
type initEvent struct {
	PermissionMode string       `json:"permissionMode"`
	Plugins        []initPlugin `json:"plugins"`
	// PluginErrors is omitted when no plugin failed to load.
	PluginErrors []json.RawMessage `json:"plugin_errors"`
}

type initPlugin struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// streamOutput is what heinzel takes from claude's stream-json output.
type streamOutput struct {
	init    *initEvent // the first system/init event, if any
	summary string     // the last event's result text
	ok      bool       // whether the last event is a successful result
}

// readStream reads a stream-json output to its end. Lines that aren't JSON
// objects are skipped.
func readStream(r io.Reader) (streamOutput, error) {
	br := bufio.NewReader(r)
	var out streamOutput
	var last *streamEvent
	for {
		line, readErr := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var e streamEvent
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				last = &e
				if out.init == nil && e.Type == "system" && e.Subtype == "init" {
					out.init = new(initEvent)
					_ = json.Unmarshal(line, out.init)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return out, readErr
		}
	}
	if last != nil && last.Type == "result" {
		out.summary, out.ok = last.Result, !last.IsError
	}
	return out, nil
}

// checkInit checks that claude's session started as heinzel configured it:
// in acceptEdits mode, with a plugin loaded from each of pluginDirs and no
// plugin errors. The init event is the only evidence that --settings and
// --plugin-dir took effect: -p silently ignores invalid settings.
func checkInit(init *initEvent, pluginDirs []string) error {
	if init == nil {
		return errors.New("no system/init event")
	}
	if init.PermissionMode != "acceptEdits" {
		return fmt.Errorf("permission mode %q, not acceptEdits", init.PermissionMode)
	}
	for _, dir := range pluginDirs {
		if !slices.ContainsFunc(init.Plugins, func(p initPlugin) bool { return loadedFrom(p.Path, dir) }) {
			return fmt.Errorf("no plugin loaded from %s", dir)
		}
	}
	if len(init.PluginErrors) > 0 {
		var msgs []string
		for _, e := range init.PluginErrors {
			msgs = append(msgs, string(e))
		}
		return fmt.Errorf("plugin errors: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// loadedFrom reports whether a plugin at path came from the --plugin-dir
// dir: the dir itself, or a plugin inside a folder of plugins.
func loadedFrom(path, dir string) bool {
	return Within(filepath.Clean(path), filepath.Clean(dir)) || Within(ResolvePath(path), ResolvePath(dir))
}

// waitOrClose waits up to timeout for wg, then closes pipes to unblock its
// readers and waits again. It reports whether wg finished in time.
func waitOrClose(wg *sync.WaitGroup, timeout time.Duration, pipes ...*os.File) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		closeAll(pipes...)
		<-done
		return false
	}
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// joinReason joins the non-blank parts of a failure reason.
func joinReason(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ": ")
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
