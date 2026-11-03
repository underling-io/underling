package recordsentryissuealert

import (
	"encoding/json"
	"errors"
	"strings"

	"underling/internal/events"
)

// The alert's details end up in Claude's prompt and in pull requests, so
// their size is bounded.
const (
	maxFrames     = 50   // kept from the newest
	maxTextRunes  = 2000 // culprit, exception type and value
	maxFrameRunes = 500  // a frame's file and function
)

// webhook is the part of Sentry's event_alert payload that is recorded.
type webhook struct {
	Data struct {
		Event struct {
			EventID string `json:"event_id"`
			IssueID string `json:"issue_id"`
			Project int64  `json:"project"`
			Title   string `json:"title"`
			Level   string `json:"level"`
			WebURL  string `json:"web_url"`
			Culprit string `json:"culprit"`
			// Decoded on its own, so that an unexpected shape drops the
			// exception details rather than the alert.
			Exception json.RawMessage `json:"exception"`
		} `json:"event"`
		TriggeredRule string `json:"triggered_rule"`
	} `json:"data"`
}

// sentryException follows Sentry's exception interface: the chained
// exceptions, innermost first, each with frames oldest call first.
type sentryException struct {
	Values []struct {
		Type       string `json:"type"`
		Value      string `json:"value"`
		Stacktrace struct {
			Frames []struct {
				Filename string `json:"filename"`
				Function string `json:"function"`
				Lineno   int    `json:"lineno"`
			} `json:"frames"`
		} `json:"stacktrace"`
	} `json:"values"`
}

// decodeAlert decodes and validates an event_alert body. Values are stored
// as sent; they are trimmed only to check they aren't blank.
func decodeAlert(body []byte) (events.SentryIssueAlertReceived, error) {
	var hook webhook
	if err := json.Unmarshal(body, &hook); err != nil {
		return events.SentryIssueAlertReceived{}, errors.New("malformed event_alert payload")
	}
	e := hook.Data.Event
	if blank(e.EventID) || blank(e.IssueID) || blank(hook.Data.TriggeredRule) {
		return events.SentryIssueAlertReceived{}, errors.New("event_id, issue_id and triggered_rule are required")
	}
	alert := events.SentryIssueAlertReceived{
		EventID:       e.EventID,
		IssueID:       e.IssueID,
		Project:       e.Project,
		Title:         e.Title,
		Level:         e.Level,
		WebURL:        e.WebURL,
		TriggeredRule: hook.Data.TriggeredRule,
		Culprit:       truncateRunes(e.Culprit, maxTextRunes),
	}
	addException(&alert, e.Exception)
	return alert, nil
}

// addException adds the outermost (last) exception's details to alert. A
// missing or oddly shaped exception adds nothing.
func addException(alert *events.SentryIssueAlertReceived, raw json.RawMessage) {
	var exc sentryException
	if len(raw) == 0 || json.Unmarshal(raw, &exc) != nil || len(exc.Values) == 0 {
		return
	}
	outer := exc.Values[len(exc.Values)-1]
	alert.ExceptionType = truncateRunes(outer.Type, maxTextRunes)
	alert.ExceptionValue = truncateRunes(outer.Value, maxTextRunes)
	frames := outer.Stacktrace.Frames
	frames = frames[max(len(frames)-maxFrames, 0):]
	for _, f := range frames {
		alert.Frames = append(alert.Frames, events.SentryStackFrame{
			File:     truncateRunes(f.Filename, maxFrameRunes),
			Function: truncateRunes(f.Function, maxFrameRunes),
			Line:     f.Lineno,
		})
	}
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	runes := 0
	for i := range s {
		if runes == n {
			return s[:i]
		}
		runes++
	}
	return s
}
