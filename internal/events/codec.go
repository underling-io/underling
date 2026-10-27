package events

import (
	"encoding/json"
	"errors"
	"fmt"

	"heinzel/internal/platform/eventstore"
)

// Event is an event contract. EventType is the type it is stored under.
type Event interface {
	EventType() string
}

func (SentryIssueAlertReceived) EventType() string { return SentryIssueAlertReceivedType }
func (SentryIssueInvestigationStarted) EventType() string {
	return SentryIssueInvestigationStartedType
}
func (SentryIssueInvestigationPullRequestOpened) EventType() string {
	return SentryIssueInvestigationPullRequestOpenedType
}
func (SentryIssueInvestigationNoChanges) EventType() string {
	return SentryIssueInvestigationNoChangesType
}
func (SentryIssueInvestigationSkipped) EventType() string {
	return SentryIssueInvestigationSkippedType
}
func (SentryIssueInvestigationFailed) EventType() string {
	return SentryIssueInvestigationFailedType
}

// decoders maps each stored type to the decoder of its contract.
var decoders = map[string]func(json.RawMessage) (Event, error){
	SentryIssueAlertReceivedType:                  decodeAs[SentryIssueAlertReceived],
	SentryIssueInvestigationStartedType:           decodeAs[SentryIssueInvestigationStarted],
	SentryIssueInvestigationPullRequestOpenedType: decodeAs[SentryIssueInvestigationPullRequestOpened],
	SentryIssueInvestigationNoChangesType:         decodeAs[SentryIssueInvestigationNoChanges],
	SentryIssueInvestigationSkippedType:           decodeAs[SentryIssueInvestigationSkipped],
	SentryIssueInvestigationFailedType:            decodeAs[SentryIssueInvestigationFailed],
}

// ErrUnknownType is returned by Decode for a type with no contract here.
var ErrUnknownType = errors.New("unknown event type")

// Encode turns e into an event to append, stored under e's own type. The
// contracts hold only strings, numbers and slices of them, so encoding
// can't fail.
func Encode(e Event) eventstore.NewEvent {
	data, _ := json.Marshal(e)
	return eventstore.NewEvent{Type: e.EventType(), Payload: data}
}

// Decode turns a stored event into its contract, a value such as
// SentryIssueAlertReceived (not a pointer).
func Decode(e eventstore.Event) (Event, error) {
	decode, ok := decoders[e.Type]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownType, e.Type)
	}
	decoded, err := decode(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", e.Type, err)
	}
	return decoded, nil
}

func decodeAs[E Event](payload json.RawMessage) (Event, error) {
	var e E
	err := json.Unmarshal(payload, &e)
	return e, err
}
