package events_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"heinzel/internal/events"
	"heinzel/internal/platform/eventstore"
)

var everyContract = []events.Event{
	events.SentryIssueAlertReceived{EventID: "e1", IssueID: "42", Project: 7, Frames: []events.SentryStackFrame{{File: "a.go", Function: "f", Line: 3}}},
	events.SentryIssueInvestigationStarted{IssueID: "42", Project: 7, Repo: "srf/app", Branch: "heinzel/42"},
	events.SentryIssueInvestigationPullRequestOpened{IssueID: "42", PRURL: "https://example.test/pr/1"},
	events.SentryIssueInvestigationNoChanges{IssueID: "42"},
	events.SentryIssueInvestigationSkipped{IssueID: "42", Reason: events.InvestigationSkippedStale},
	events.SentryIssueInvestigationFailed{IssueID: "42", Stage: events.InvestigationFailedClone, Reason: "boom"},
}

func stored(e eventstore.NewEvent) eventstore.Event {
	return eventstore.Event{Type: e.Type, Payload: e.Payload}
}

func TestEveryContractRoundTripsThroughEncodeAndDecode(t *testing.T) {
	for _, contract := range everyContract {
		t.Run(contract.EventType(), func(t *testing.T) {
			got, err := events.Decode(stored(events.Encode(contract)))

			if err != nil || !reflect.DeepEqual(got, contract) {
				t.Fatalf("got %#v, %v; want %#v", got, err, contract)
			}
		})
	}
}

func TestEncodeStoresAContractUnderItsTypeConstant(t *testing.T) {
	got := events.Encode(events.SentryIssueInvestigationSkipped{IssueID: "42", Reason: "stale"})

	if got.Type != events.SentryIssueInvestigationSkippedType || string(got.Payload) != `{"issueId":"42","reason":"stale"}` {
		t.Fatalf("got %s %s", got.Type, got.Payload)
	}
}

func TestDecodingAnUnknownTypeFails(t *testing.T) {
	_, err := events.Decode(eventstore.Event{Type: "NoteRegistered", Payload: json.RawMessage(`{}`)})

	if !errors.Is(err, events.ErrUnknownType) {
		t.Fatalf("got %v, want ErrUnknownType", err)
	}
}

func TestDecodingAMalformedPayloadFails(t *testing.T) {
	_, err := events.Decode(eventstore.Event{Type: events.SentryIssueAlertReceivedType, Payload: json.RawMessage(`{"issueId":1}`)})

	if err == nil || errors.Is(err, events.ErrUnknownType) {
		t.Fatalf("got %v, want a decode error", err)
	}
}
