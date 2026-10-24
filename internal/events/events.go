// Package events holds the event contracts shared between features: type
// names and payload shapes. It holds no behaviour. The JSON shapes are
// persisted, so changing them is a breaking change.
package events

// SentryIssueAlertReceivedType is the stored type of a SentryIssueAlertReceived event.
const SentryIssueAlertReceivedType = "SentryIssueAlertReceived"

// SentryIssueAlertReceived records that a Sentry issue alert rule fired for
// one Sentry event.
type SentryIssueAlertReceived struct {
	EventID       string `json:"eventId"`
	IssueID       string `json:"issueId"`
	Project       int64  `json:"project"`
	Title         string `json:"title"`
	Level         string `json:"level"`
	WebURL        string `json:"webUrl"`
	TriggeredRule string `json:"triggeredRule"`
}
