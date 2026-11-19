package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// waitDelay bounds how long Wait waits for the output pipes after the
	// child was killed, e.g. while a grandchild still holds them.
	waitDelay = 10 * time.Second
	// drainTimeout bounds how long the output is read after the child
	// exited and its process group was killed.
	drainTimeout = 5 * time.Second
	// stderrTail is how much of the child's stderr a failure keeps.
	stderrTail = 2 << 10
)

// TimeoutError is what Run returns when claude ran past its timeout.
type TimeoutError struct {
	After time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("claude did not finish within %v", e.After)
}

// Timeout reports true, like net and os timeout errors.
func (*TimeoutError) Timeout() bool { return true }

// Run runs cmd until it exits, timeout passes or ctx is cancelled; in the
// latter cases it kills claude and its process group. It returns Claude's
// summary, or why the run failed: a *TimeoutError after the timeout.
func Run(ctx context.Context, cmd Cmd, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, cmd.bin, cmd.args...)
	c.Dir = cmd.dir
	c.Env = cmd.env
	c.WaitDelay = waitDelay
	killProcessGroupOnCancel(c)

	// underling makes the pipes itself and hands the child its ends, so Wait
	// returns as soon as the child exits, even while a tool it left behind
	// still holds them.
	p, err := newPipes()
	if err != nil {
		return "", errors.New("create pipe: " + err.Error())
	}
	defer p.closeParentEnds()
	c.Stdin, c.Stdout, c.Stderr = p.stdinR, p.stdoutW, p.stderrW
	startErr := c.Start()
	p.closeChildEnds()
	if startErr != nil {
		return "", errors.New("start claude: " + startErr.Error())
	}

	var (
		out     streamOutput
		readErr error
		stderr  = &tailBuffer{max: stderrTail}
		copying sync.WaitGroup
	)
	copying.Go(func() {
		_, _ = io.WriteString(p.stdinW, cmd.stdin)
		_ = p.stdinW.Close()
	})
	copying.Go(func() { out, readErr = readStream(p.stdoutR) })
	copying.Go(func() { _, _ = io.Copy(stderr, p.stderrR) })

	waitErr := c.Wait()
	// Kill whatever the child left in its process group, so no tool
	// outlives the run or overlaps the push.
	killGroup(c)
	if !waitOrClose(&copying, drainTimeout, p.parentEnds()...) && errors.Is(readErr, os.ErrClosed) {
		readErr = nil // a process outside the group still held the pipe
	}

	switch {
	case ctx.Err() != nil:
		return "", errors.New("underling shut down while claude was running")
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return "", &TimeoutError{After: timeout}
	case waitErr != nil:
		return "", errors.New(joinReason("claude: "+waitErr.Error(), out.summary, stderr.String()))
	case readErr != nil:
		return "", errors.New("read claude output: " + readErr.Error())
	}
	if err := checkInit(out.init, cmd.permissionMode, cmd.pluginDirs); err != nil {
		return "", errors.New("claude's session did not start as configured: " + err.Error())
	}
	if !out.ok {
		return "", errors.New(joinReason("claude did not end with a successful result", out.summary, stderr.String()))
	}
	return out.summary, nil
}

// pipes are the child's stdio pipes, both ends.
type pipes struct {
	stdinR, stdinW   *os.File
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
}

func newPipes() (*pipes, error) {
	var p pipes
	for _, pair := range [][2]**os.File{{&p.stdinR, &p.stdinW}, {&p.stdoutR, &p.stdoutW}, {&p.stderrR, &p.stderrW}} {
		r, w, err := os.Pipe()
		if err != nil {
			p.closeChildEnds()
			p.closeParentEnds()
			return nil, err
		}
		*pair[0], *pair[1] = r, w
	}
	return &p, nil
}

func (p *pipes) childEnds() []*os.File  { return []*os.File{p.stdinR, p.stdoutW, p.stderrW} }
func (p *pipes) parentEnds() []*os.File { return []*os.File{p.stdinW, p.stdoutR, p.stderrR} }
func (p *pipes) closeChildEnds()        { closeAll(p.childEnds()...) }
func (p *pipes) closeParentEnds()       { closeAll(p.parentEnds()...) }

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// waitOrClose waits up to timeout for wg, then closes files to unblock its
// readers and waits again. It reports whether wg finished in time.
func waitOrClose(wg *sync.WaitGroup, timeout time.Duration, files ...*os.File) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		closeAll(files...)
		<-done
		return false
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
	b.buf = b.buf[max(len(b.buf)-b.max, 0):]
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
