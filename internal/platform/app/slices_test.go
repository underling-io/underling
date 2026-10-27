package app

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
)

func TestBuildKeepsRegistrationOrderWithinEachKind(t *testing.T) {
	var r registry
	for _, path := range []string{"/a", "/b"} {
		r.stateChanges = append(r.stateChanges, func(Deps) (StateChange, error) { return StateChange{Path: path}, nil })
	}
	var built []string
	for _, name := range []string{"first", "second"} {
		r.automations = append(r.automations, func(Deps) (Automation, error) {
			built = append(built, name)
			return Automation{}, nil
		})
	}

	got, err := r.build(Deps{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var paths []string
	for _, sc := range got.StateChanges {
		paths = append(paths, sc.Path)
	}
	if !slices.Equal(paths, []string{"/a", "/b"}) || len(got.Automations) != 2 || !slices.Equal(built, []string{"first", "second"}) {
		t.Errorf("got paths %v, %d automations built in order %v", paths, len(got.Automations), built)
	}
}

func TestBuildPassesTheDepsToEveryFactory(t *testing.T) {
	var r registry
	logger := slog.New(slog.DiscardHandler)
	var seen []Deps
	r.stateChanges = append(r.stateChanges, func(d Deps) (StateChange, error) { seen = append(seen, d); return StateChange{}, nil })
	r.automations = append(r.automations, func(d Deps) (Automation, error) { seen = append(seen, d); return Automation{}, nil })

	if _, err := r.build(Deps{Getenv: func(string) string { return "value" }, Logger: logger}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("factories called %d times, want 2", len(seen))
	}
	for _, d := range seen {
		if d.Logger != logger || d.Getenv("X") != "value" {
			t.Errorf("factory got %+v, want the deps passed to build", d)
		}
	}
}

func TestBuildStopsAtTheFirstErrorAndReturnsItUnwrapped(t *testing.T) {
	var r registry
	errFirst := errors.New("SENTRY_CLIENT_SECRET must be set")
	laterCalled := false
	r.stateChanges = append(r.stateChanges,
		func(Deps) (StateChange, error) { return StateChange{}, errFirst },
		func(Deps) (StateChange, error) { laterCalled = true; return StateChange{}, nil })
	r.automations = append(r.automations, func(Deps) (Automation, error) { laterCalled = true; return Automation{}, nil })

	if _, err := r.build(Deps{}); err != errFirst {
		t.Errorf("got %v, want %v unwrapped", err, errFirst)
	}
	if laterCalled {
		t.Error("factories after the failing one were called")
	}
}
