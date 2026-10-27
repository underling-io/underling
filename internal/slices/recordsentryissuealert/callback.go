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

	"github.com/gin-gonic/gin"

	"heinzel/internal/events"
	"heinzel/internal/platform/eventstore"
)

const maxBodyBytes = 10 << 20

// callback handles the "Record Sentry issue alert" command.
type callback struct {
	secret []byte
	store  *eventstore.Store
	logger *slog.Logger
}

type statusReply struct {
	Status string `json:"status"`
}

type errorReply struct {
	Error string `json:"error"`
}

func (cb *callback) handle(c *gin.Context) {
	status, reply := cb.record(c.Request, unwrap(c.Writer))
	body, _ := json.Marshal(reply)
	// c.Data rather than c.JSON, which would add "; charset=utf-8".
	c.Data(status, "application/json", body)
}

// unwrap returns the writer under Gin's. Only given net/http's own writer
// does http.MaxBytesReader close the connection after a too-large body.
func unwrap(w gin.ResponseWriter) http.ResponseWriter {
	if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		return u.Unwrap()
	}
	return w
}

// record runs the checks in the order the event model specifies: size,
// signature, resource, validation, then the append that dedupes.
func (cb *callback) record(r *http.Request, w http.ResponseWriter) (int, any) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, errorReply{"body too large"}
	}
	if err != nil {
		return http.StatusBadRequest, errorReply{"cannot read body"}
	}
	if !cb.signed(r.Header.Get("Sentry-Hook-Signature"), body) {
		return http.StatusUnauthorized, errorReply{"invalid signature"}
	}
	if r.Header.Get("Sentry-Hook-Resource") != "event_alert" {
		return http.StatusOK, statusReply{"ignored"}
	}
	alert, err := decodeAlert(body)
	if err != nil {
		return http.StatusBadRequest, errorReply{err.Error()}
	}
	payload, err := json.Marshal(alert)
	if err != nil {
		return cb.internalError(err)
	}
	received := eventstore.NewEvent{Type: events.SentryIssueAlertReceivedType, Payload: payload}
	_, _, err = cb.store.Append(streamOf(alert), 0, []eventstore.NewEvent{received})
	switch {
	case errors.Is(err, eventstore.ErrConflict):
		return http.StatusOK, statusReply{"duplicate"}
	case err != nil:
		return cb.internalError(err)
	}
	return http.StatusOK, statusReply{"recorded"}
}

// signed reports whether signature is the hex HMAC-SHA256 of body, keyed
// with the client secret.
func (cb *callback) signed(signature string, body []byte) bool {
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, cb.secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// streamOf names the stream of one alert: one Sentry event triggering one
// rule. Both are free text, so they are hashed rather than joined.
func streamOf(alert events.SentryIssueAlertReceived) string {
	sum := sha256.Sum256([]byte(alert.EventID + "\x00" + alert.TriggeredRule))
	return "sentry-issue-alert-" + hex.EncodeToString(sum[:])
}

func (cb *callback) internalError(err error) (int, any) {
	cb.logger.Error("record sentry issue alert", "error", err)
	return http.StatusInternalServerError, errorReply{"internal error"}
}
