package app

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
)

func TestBuildKeepsRegistrationOrderWithinEachKind(t *testing.T) {
	var r registry
	for _, pattern := range []string{"GET /a", "GET /b"} {
		r.registerStateChange(func(Deps) (StateChange, error) {
			return StateChange{Pattern: pattern}, nil
		})
	}
	var built []string
	for _, name := range []string{"first", "second"} {
		r.registerAutomation(func(Deps) (Automation, error) {
			built = append(built, name)
			return Automation{}, nil
		})
	}

	got, err := r.build(Deps{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var patterns []string
	for _, sc := range got.StateChanges {
		patterns = append(patterns, sc.Pattern)
	}
	if want := []string{"GET /a", "GET /b"}; !slices.Equal(patterns, want) {
		t.Errorf("state change patterns = %v, want %v", patterns, want)
	}
	if len(got.Automations) != 2 {
		t.Errorf("got %d automations, want 2", len(got.Automations))
	}
	if want := []string{"first", "second"}; !slices.Equal(built, want) {
		t.Errorf("automation factories called in order %v, want %v", built, want)
	}
}

func TestBuildPassesDepsToFactories(t *testing.T) {
	var r registry
	logger := slog.New(slog.DiscardHandler)
	deps := Deps{Getenv: func(string) string { return "value" }, Logger: logger}
	var seen []Deps
	r.registerStateChange(func(d Deps) (StateChange, error) {
		seen = append(seen, d)
		return StateChange{}, nil
	})
	r.registerAutomation(func(d Deps) (Automation, error) {
		seen = append(seen, d)
		return Automation{}, nil
	})

	if _, err := r.build(deps); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("factories called %d times, want 2", len(seen))
	}
	for _, d := range seen {
		if d.Logger != logger || d.Getenv("X") != "value" {
			t.Errorf("factory got deps %+v, want the ones passed to build", d)
		}
	}
}

func TestBuildReturnsFirstErrorUnwrappedAndStops(t *testing.T) {
	var r registry
	errFirst := errors.New("SENTRY_CLIENT_SECRET must be set")
	laterCalled := false
	r.registerStateChange(func(Deps) (StateChange, error) {
		return StateChange{}, errFirst
	})
	r.registerStateChange(func(Deps) (StateChange, error) {
		laterCalled = true
		return StateChange{}, errors.New("second")
	})
	r.registerAutomation(func(Deps) (Automation, error) {
		laterCalled = true
		return Automation{}, errors.New("third")
	})

	_, err := r.build(Deps{})
	if err != errFirst {
		t.Errorf("build error = %v, want %v unwrapped", err, errFirst)
	}
	if laterCalled {
		t.Error("factories after the failing one were called")
	}
}

func TestBuildWithNothingRegisteredGivesEmptySlices(t *testing.T) {
	var r registry
	got, err := r.build(Deps{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(got.StateChanges) != 0 || len(got.Automations) != 0 {
		t.Errorf("build = %+v, want empty slices", got)
	}
}
