// Package events holds the event contracts shared between slices: type
// names and payload shapes, plus Encode and Decode, which tie each payload
// to its type name. It holds no other behaviour. The JSON shapes are
// persisted, so changing them is a breaking change.
package events

// SentryIssueAlertReceivedType is the stored type of a SentryIssueAlertReceived event.
const SentryIssueAlertReceivedType = "SentryIssueAlertReceived"

// SentryIssueAlertReceived records that a Sentry issue alert rule fired for
// one Sentry event. The exception details are optional: alerts recorded
// before they existed, and events without an exception, leave them empty.
type SentryIssueAlertReceived struct {
	EventID       string `json:"eventId"`
	IssueID       string `json:"issueId"`
	Project       int64  `json:"project"`
	Title         string `json:"title"`
	Level         string `json:"level"`
	WebURL        string `json:"webUrl"`
	TriggeredRule string `json:"triggeredRule"`
	Culprit       string `json:"culprit,omitempty"`
	// ExceptionType, ExceptionValue and Frames come from the outermost
	// (last) exception Sentry sent. Frames are in Sentry's order, oldest
	// call first, keeping at most the last 50.
	ExceptionType  string             `json:"exceptionType,omitempty"`
	ExceptionValue string             `json:"exceptionValue,omitempty"`
	Frames         []SentryStackFrame `json:"frames,omitempty"`
}

// SentryStackFrame is one stack frame of a Sentry exception.
type SentryStackFrame struct {
	File     string `json:"file"`
	Function string `json:"function"`
	Line     int    `json:"line"`
}

// Stored types of the Sentry issue investigation events. All of them go on
// the stream of one Sentry issue: its first event (Started or Skipped) at
// version 1, and the outcome after Started at version 2.
const (
	SentryIssueInvestigationStartedType           = "SentryIssueInvestigationStarted"
	SentryIssueInvestigationPullRequestOpenedType = "SentryIssueInvestigationPullRequestOpened"
	SentryIssueInvestigationNoChangesType         = "SentryIssueInvestigationNoChanges"
	SentryIssueInvestigationSkippedType           = "SentryIssueInvestigationSkipped"
	SentryIssueInvestigationFailedType            = "SentryIssueInvestigationFailed"
)

// SentryIssueInvestigationStarted records that an investigation of a Sentry
// issue began: Repo ("owner/repo") is cloned and Claude works on Branch.
type SentryIssueInvestigationStarted struct {
	IssueID string `json:"issueId"`
	Project int64  `json:"project"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
}

// SentryIssueInvestigationPullRequestOpened records the draft PR an
// investigation opened.
type SentryIssueInvestigationPullRequestOpened struct {
	IssueID string `json:"issueId"`
	PRURL   string `json:"prUrl"`
}

// SentryIssueInvestigationNoChanges records that an investigation ended
// without new commits, so no PR was opened.
type SentryIssueInvestigationNoChanges struct {
	IssueID string `json:"issueId"`
}

// Reasons of a SentryIssueInvestigationSkipped.
const (
	InvestigationSkippedUnmapped = "unmapped" // no repo is mapped to the alert's project
	InvestigationSkippedStale    = "stale"    // the alert is older than the max age
)

// SentryIssueInvestigationSkipped records that a Sentry issue was not
// investigated, for Reason.
type SentryIssueInvestigationSkipped struct {
	IssueID string `json:"issueId"`
	Reason  string `json:"reason"`
}

// Stages of a SentryIssueInvestigationFailed.
const (
	InvestigationFailedClone    = "clone"
	InvestigationFailedClaude   = "claude"
	InvestigationFailedTimeout  = "timeout"
	InvestigationFailedPush     = "push"
	InvestigationFailedPR       = "pr"
	InvestigationFailedShutdown = "shutdown"
)

// SentryIssueInvestigationFailed records that an investigation failed at
// Stage, with a human-readable Reason. It is final: there is no retry.
type SentryIssueInvestigationFailed struct {
	IssueID string `json:"issueId"`
	Stage   string `json:"stage"`
	Reason  string `json:"reason"`
}
