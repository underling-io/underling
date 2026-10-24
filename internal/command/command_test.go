package command_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"heinzel/internal/command"
	"heinzel/internal/eventstore"
)

func TestRegisterNoteDecidesNoteRegisteredOnNewStream(t *testing.T) {
	r := command.DefaultRegistry()

	got, err := r.Decide(context.Background(), "RegisterNote", json.RawMessage(`{"id":"n1","text":"hello"}`))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	want := command.Decision{
		StreamID:        "n1",
		ExpectedVersion: 0,
		Events: []eventstore.NewEvent{{
			Type:    "NoteRegistered",
			Payload: json.RawMessage(`{"id":"n1","text":"hello"}`),
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRegisterNoteRejectsInvalidPayloads(t *testing.T) {
	cases := map[string]string{
		"empty id":         `{"id":"","text":"hello"}`,
		"blank id":         `{"id":"  ","text":"hello"}`,
		"missing id":       `{"text":"hello"}`,
		"empty text":       `{"id":"n1","text":""}`,
		"blank text":       `{"id":"n1","text":"\t\n "}`,
		"missing text":     `{"id":"n1"}`,
		"wrong field type": `{"id":1,"text":"hello"}`,
	}
	r := command.DefaultRegistry()
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.Decide(context.Background(), "RegisterNote", json.RawMessage(payload))
			if !errors.Is(err, command.ErrInvalid) {
				t.Fatalf("got err %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDecideRejectsUnknownCommandType(t *testing.T) {
	r := command.DefaultRegistry()

	for _, commandType := range []string{"", "Nope"} {
		_, err := r.Decide(context.Background(), commandType, json.RawMessage(`{}`))
		if !errors.Is(err, command.ErrInvalid) {
			t.Fatalf("type %q: got err %v, want ErrInvalid", commandType, err)
		}
	}
}

func TestRegistryAcceptsAdditionalCommands(t *testing.T) {
	r := command.NewRegistry()
	r.Register("Ping", func(_ context.Context, _ json.RawMessage) (command.Decision, error) {
		return command.Decision{StreamID: "ping"}, nil
	})

	got, err := r.Decide(context.Background(), "Ping", json.RawMessage(`{}`))
	if err != nil || got.StreamID != "ping" {
		t.Fatalf("got %+v err %v, want stream ping", got, err)
	}
}
