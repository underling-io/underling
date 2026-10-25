package recordsentryissuealert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"heinzel/internal/events"
	"heinzel/internal/eventstore"
)

const maxBodyBytes = 10 << 20

// callback serves POST /sentry/callback: the "Record Sentry issue alert" command.
type callback struct {
	secret []byte
	store  *eventstore.Store
	logger *slog.Logger
}

// webhook is the part of Sentry's event_alert payload we record.
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
			// Exception is decoded leniently by exceptionDetails, so an
			// unexpected shape drops the details instead of the alert.
			Exception json.RawMessage `json:"exception"`
		} `json:"event"`
		TriggeredRule string `json:"triggered_rule"`
	} `json:"data"`
}

type statusResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "body too large"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "cannot read body"})
		return
	}
	if !c.validSignature(r.Header.Get("Sentry-Hook-Signature"), body) {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid signature"})
		return
	}
	if r.Header.Get("Sentry-Hook-Resource") != "event_alert" {
		writeJSON(w, http.StatusOK, statusResponse{Status: "ignored"})
		return
	}
	var hook webhook
	if err := json.Unmarshal(body, &hook); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "malformed event_alert payload"})
		return
	}
	event := hook.Data.Event
	if blank(event.EventID) || blank(event.IssueID) || blank(hook.Data.TriggeredRule) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "event_id, issue_id and triggered_rule are required"})
		return
	}
	exception := exceptionDetails(event.Exception)
	payload, err := json.Marshal(events.SentryIssueAlertReceived{
		EventID:        event.EventID,
		IssueID:        event.IssueID,
		Project:        event.Project,
		Title:          event.Title,
		Level:          event.Level,
		WebURL:         event.WebURL,
		TriggeredRule:  hook.Data.TriggeredRule,
		Culprit:        culprit(event.Culprit),
		ExceptionType:  exception.Type,
		ExceptionValue: exception.Value,
		Frames:         exception.Frames,
	})
	if err != nil {
		c.internalError(w, err)
		return
	}
	newEvent := eventstore.NewEvent{Type: events.SentryIssueAlertReceivedType, Payload: payload}
	_, _, err = c.store.Append(streamID(event.EventID, hook.Data.TriggeredRule), 0, []eventstore.NewEvent{newEvent})
	switch {
	case errors.Is(err, eventstore.ErrConflict):
		writeJSON(w, http.StatusOK, statusResponse{Status: "duplicate"})
	case err != nil:
		c.internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, statusResponse{Status: "recorded"})
	}
}

// blank reports whether s is empty or only whitespace. Values are trimmed
// only to validate them; they are stored as sent.
func blank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// validSignature reports whether signature is the hex HMAC-SHA256 of body
// keyed with the client secret.
func (c *callback) validSignature(signature string, body []byte) bool {
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// streamID names the stream of one alert: one Sentry event triggering one
// rule. Both parts are free text, so they are hashed rather than joined.
func streamID(eventID, rule string) string {
	sum := sha256.Sum256([]byte(eventID + "\x00" + rule))
	return "sentry-issue-alert-" + hex.EncodeToString(sum[:])
}

func (c *callback) internalError(w http.ResponseWriter, err error) {
	c.logger.Error("record sentry issue alert", "error", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
