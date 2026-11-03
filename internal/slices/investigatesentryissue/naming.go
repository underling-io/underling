package investigatesentryissue

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

var plainIssueID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// issueKey is the issue id as stream and branch names use it: as-is if it
// is [A-Za-z0-9_-]+, otherwise its hex sha256.
func issueKey(issueID string) string {
	if plainIssueID.MatchString(issueID) {
		return issueID
	}
	sum := sha256.Sum256([]byte(issueID))
	return hex.EncodeToString(sum[:])
}

// streamID names the stream holding every investigation event of an issue.
func streamID(issueID string) string {
	return "sentry-issue-investigation-" + issueKey(issueID)
}

// branchName names the branch Claude works on for an issue.
func branchName(issueID string) string {
	return "underling/sentry-" + issueKey(issueID)
}
