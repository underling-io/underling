package investigatesentryissue

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"underling/internal/slices/investigatesentryissue/claude"
)

// The GWT tests fake a timeout with their own error type; this checks that
// the error a real claude run returns is spotted the same way.
func TestARealClaudeTimeoutCountsAsTimedOut(t *testing.T) {
	err := fmt.Errorf("run claude: %w", &claude.TimeoutError{After: 30 * time.Minute})
	if !timedOut(err) {
		t.Errorf("timedOut(%v) = false, want true", err)
	}
}

func TestAClaudeFailureDoesNotCountAsTimedOut(t *testing.T) {
	err := errors.New("claude: exit status 1")
	if timedOut(err) {
		t.Errorf("timedOut(%v) = true, want false", err)
	}
}
