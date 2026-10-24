// Package command turns commands into the events they produce.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"heinzel/internal/eventstore"
)

// ErrInvalid marks a command that was rejected before reaching the store:
// an unknown type or a payload that failed decoding or validation.
var ErrInvalid = errors.New("invalid command")

// Invalid returns an error wrapping ErrInvalid with a client-facing message.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Decision is what a command handler wants appended to the store.
type Decision struct {
	StreamID        string
	ExpectedVersion uint64
	Events          []eventstore.NewEvent
}

// Handler decodes and validates a command payload and decides which events to append.
type Handler func(ctx context.Context, payload json.RawMessage) (Decision, error)

// Registry maps command types to their handlers.
type Registry struct {
	handlers map[string]Handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// DefaultRegistry returns a registry with all built-in commands.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("RegisterNote", RegisterNote)
	return r
}

// Register adds or replaces the handler for commandType.
func (r *Registry) Register(commandType string, h Handler) {
	r.handlers[commandType] = h
}

// Decide runs the handler registered for commandType.
func (r *Registry) Decide(ctx context.Context, commandType string, payload json.RawMessage) (Decision, error) {
	h, ok := r.handlers[commandType]
	if !ok {
		return Decision{}, Invalid("unknown command type %q", commandType)
	}
	return h(ctx, payload)
}
