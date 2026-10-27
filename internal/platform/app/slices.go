// Package app wires the slices to the event store, serves the state
// changes' routes with Gin, runs the automations' event handlers, and owns
// their lifecycle.
package app

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"

	"heinzel/internal/platform/eventhandler"
	"heinzel/internal/platform/eventstore"
)

// StateChange is a state-change slice: one HTTP route that may append to
// the store. Handler is called once at startup.
type StateChange struct {
	Method  string // e.g. http.MethodPost
	Path    string // a Gin path, e.g. /sentry/callback or /notes/:id
	Handler func(store *eventstore.Store) gin.HandlerFunc
}

// Automation is an automation slice: an event handler reacting to stored
// events. Handler is called once at startup, after the store is open. Its
// ctx is cancelled when shutdown begins, before the event handlers stop,
// so a long-running Handle can abort.
type Automation struct {
	Handler func(ctx context.Context, store *eventstore.Store) eventhandler.Handler
}

// Slices is every slice the app runs, by kind.
type Slices struct {
	StateChanges []StateChange
	Automations  []Automation
}

// Deps is what the app hands each slice factory at startup.
type Deps struct {
	Getenv func(string) string // like os.Getenv
	Logger *slog.Logger
}

type registry struct {
	stateChanges []func(Deps) (StateChange, error)
	automations  []func(Deps) (Automation, error)
}

var registered registry

// RegisterStateChange adds a state-change factory. Slices call it from init.
func RegisterStateChange(factory func(Deps) (StateChange, error)) {
	registered.stateChanges = append(registered.stateChanges, factory)
}

// RegisterAutomation adds an automation factory. Slices call it from init.
func RegisterAutomation(factory func(Deps) (Automation, error)) {
	registered.automations = append(registered.automations, factory)
}

// Build calls every registered factory with d, in registration order
// within each kind. It stops at the first error and returns it unwrapped.
func Build(d Deps) (Slices, error) {
	return registered.build(d)
}

func (r *registry) build(d Deps) (Slices, error) {
	var s Slices
	for _, factory := range r.stateChanges {
		sc, err := factory(d)
		if err != nil {
			return Slices{}, err
		}
		s.StateChanges = append(s.StateChanges, sc)
	}
	for _, factory := range r.automations {
		a, err := factory(d)
		if err != nil {
			return Slices{}, err
		}
		s.Automations = append(s.Automations, a)
	}
	return s, nil
}
