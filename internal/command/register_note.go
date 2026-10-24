package command

import (
	"context"
	"encoding/json"
	"strings"

	"heinzel/internal/eventstore"
)

type note struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// RegisterNote creates the note stream named by the payload's id with a
// single NoteRegistered event. The stream must not exist yet.
func RegisterNote(_ context.Context, payload json.RawMessage) (Decision, error) {
	var n note
	if err := json.Unmarshal(payload, &n); err != nil {
		return Decision{}, Invalid("decode RegisterNote payload: %v", err)
	}
	if strings.TrimSpace(n.ID) == "" {
		return Decision{}, Invalid("id must not be empty")
	}
	if strings.TrimSpace(n.Text) == "" {
		return Decision{}, Invalid("text must not be empty")
	}
	data, err := json.Marshal(n)
	if err != nil {
		return Decision{}, err
	}
	return Decision{
		StreamID:        n.ID,
		ExpectedVersion: 0,
		Events:          []eventstore.NewEvent{{Type: "NoteRegistered", Payload: data}},
	}, nil
}
