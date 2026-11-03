package investigatesentryissue

import "testing"

func TestStreamAndBranchNames(t *testing.T) {
	tests := []struct {
		issueID, stream, branch string
	}{
		{"1117540176", "sentry-issue-investigation-1117540176", "underling/sentry-1117540176"},
		{"PROJ-1_a", "sentry-issue-investigation-PROJ-1_a", "underling/sentry-PROJ-1_a"},
		// sha256("a/b")
		{"a/b", "sentry-issue-investigation-c14cddc033f64b9dea80ea675cf280a015e672516090a5626781153dc68fea11", "underling/sentry-c14cddc033f64b9dea80ea675cf280a015e672516090a5626781153dc68fea11"},
	}
	for _, tt := range tests {
		if got := streamID(tt.issueID); got != tt.stream {
			t.Errorf("streamID(%q) = %q, want %q", tt.issueID, got, tt.stream)
		}
		if got := branchName(tt.issueID); got != tt.branch {
			t.Errorf("branchName(%q) = %q, want %q", tt.issueID, got, tt.branch)
		}
	}
}
