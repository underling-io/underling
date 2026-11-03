package slices_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"underling/internal/platform/app"
	_ "underling/internal/slices" // registers every slice, as main does
)

const (
	e2eSecret  = "e2e-client-secret"
	e2eTimeout = 5 * time.Second
)

// pipeListener is an in-memory net.Listener: Dial hands it the server end of
// a net.Pipe, so the app can be served without binding a socket.
type pipeListener struct {
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close unblocks Accept with net.ErrClosed. It is safe to call more than once.
func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// Dial returns the client end of a new pipe whose server end Accept returns.
func (l *pipeListener) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		_, _ = client.Close(), server.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_, _ = client.Close(), server.Close()
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// logSink collects the JSON log records written to it, one per Write, and
// signals each arrival. It is safe for concurrent use.
type logSink struct {
	mu      sync.Mutex
	records []map[string]any
	arrived chan struct{}
}

func newLogSink() *logSink {
	return &logSink{arrived: make(chan struct{}, 1)}
}

func (s *logSink) Write(p []byte) (int, error) {
	var record map[string]any
	if err := json.Unmarshal(p, &record); err != nil {
		record = map[string]any{"undecodable": string(p)}
	}
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	select {
	case s.arrived <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (s *logSink) snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.records...)
}

// waitFor returns the first record matching match, failing the test if none
// arrives within e2eTimeout.
func (s *logSink) waitFor(t *testing.T, match func(map[string]any) bool) map[string]any {
	t.Helper()
	timeout := time.NewTimer(e2eTimeout)
	defer timeout.Stop()
	for {
		for _, record := range s.snapshot() {
			if match(record) {
				return record
			}
		}
		select {
		case <-s.arrived:
		case <-timeout.C:
			t.Fatalf("no matching log record within %v; records seen: %v", e2eTimeout, s.snapshot())
			return nil
		}
	}
}

// startApp builds the slices the way main does and runs the app on an
// in-memory listener with a fresh data dir. It returns a client connected to
// the app. Cleanup stops the app and requires a clean shutdown.
func startApp(t *testing.T, logger *slog.Logger) *http.Client {
	t.Helper()
	env := map[string]string{
		"SENTRY_CLIENT_SECRET": e2eSecret,
		"SENTRY_GITHUB_REPOS":  "2=acme/web",
		"GITHUB_TOKEN":         "e2e-github-token",
		"ANTHROPIC_API_KEY":    "e2e-anthropic-key",
		"OPENAI_API_KEY":       "e2e-openai-key",
		"CLAUDE_PLUGIN_DIRS":   "/plugins/codex,/plugins/auto-task",
	}
	slices, err := app.Build(app.Deps{
		Getenv: func(key string) string { return env[key] },
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}

	cfg := app.Config{DataDir: t.TempDir(), ShutdownTimeout: e2eTimeout}
	ln := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx, cfg, ln, slices, logger) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("app.Run returned %v, want nil", err)
			}
		case <-time.After(2 * e2eTimeout):
			t.Errorf("app.Run did not return within %v of cancellation", 2*e2eTimeout)
		}
	})

	return &http.Client{
		Timeout:   e2eTimeout,
		Transport: &http.Transport{DialContext: ln.Dial, DisableKeepAlives: true},
	}
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(e2eSecret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSentryIssueAlertIsRecordedAndHandled(t *testing.T) {
	sink := newLogSink()
	client := startApp(t, slog.New(slog.NewJSONHandler(sink, nil)))

	const issueID = "1117540176"
	body := `{"action":"triggered","data":{"event":{"event_id":"aa1deb2fc8ee4d5d9f2a6a2a4ae6c4c4","issue_id":"` + issueID +
		`","project":1,"title":"ReferenceError: blooopy is not defined","level":"error"},"triggered_rule":"Very Important Alert Rule!"}}`
	req, err := http.NewRequest(http.MethodPost, "http://underling.test/sentry/callback", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", "event_alert")
	req.Header.Set("Sentry-Hook-Signature", sign(body))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /sentry/callback: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var got struct {
		Status string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Status != "recorded" {
		t.Fatalf("got %d %s, want 200 {\"status\":\"recorded\"}", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	// Project 1 is not in SENTRY_GITHUB_REPOS, so the investigator skips the
	// issue without cloning, running claude or calling GitHub.
	sink.waitFor(t, func(record map[string]any) bool {
		return record["msg"] == "sentry issue investigation finished" &&
			record["handler"] == "sentry-issue-investigator" &&
			record["issueId"] == issueID &&
			record["outcome"] == "skipped" &&
			record["reason"] == "unmapped"
	})
}
