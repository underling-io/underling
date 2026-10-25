package recordsentryissuealert

import (
	"encoding/json"

	"heinzel/internal/events"
)

const (
	// maxFrames is how many stack frames are kept, counted from the newest.
	maxFrames = 50
	// maxTextRunes caps the culprit and the exception type and value.
	maxTextRunes = 2000
	// maxFrameRunes caps a frame's file and function.
	maxFrameRunes = 500
)

// truncateRunes returns s cut to at most n runes.
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// culprit returns Sentry's culprit, capped in length.
func culprit(s string) string {
	return truncateRunes(s, maxTextRunes)
}

// sentryException is the part of Sentry's data.event.exception we record.
// Frame keys follow Sentry's stack trace interface: filename, function and
// lineno, ordered oldest call first.
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

type exception struct {
	Type   string
	Value  string
	Frames []events.SentryStackFrame
}

// exceptionDetails returns the outermost (last) exception in raw, with at
// most its last maxFrames frames and every text capped in length. Alert
// details end up in Claude's prompt and in pull requests, so their size
// stays bounded. A missing or unexpectedly shaped
// exception gives empty details: they are optional, so they never reject
// an alert.
func exceptionDetails(raw json.RawMessage) exception {
	if len(raw) == 0 {
		return exception{}
	}
	var decoded sentryException
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Values) == 0 {
		return exception{}
	}
	outer := decoded.Values[len(decoded.Values)-1]
	frames := outer.Stacktrace.Frames
	if len(frames) > maxFrames {
		frames = frames[len(frames)-maxFrames:]
	}
	var kept []events.SentryStackFrame
	for _, f := range frames {
		kept = append(kept, events.SentryStackFrame{
			File:     truncateRunes(f.Filename, maxFrameRunes),
			Function: truncateRunes(f.Function, maxFrameRunes),
			Line:     f.Lineno,
		})
	}
	return exception{
		Type:   truncateRunes(outer.Type, maxTextRunes),
		Value:  truncateRunes(outer.Value, maxTextRunes),
		Frames: kept,
	}
}
