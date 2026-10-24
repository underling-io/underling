package app

import "log/slog"

// Deps is what the app hands every slice factory at startup.
type Deps struct {
	// Getenv looks up an environment variable, like os.Getenv.
	Getenv func(string) string
	Logger *slog.Logger
}

// registry holds the slice factories in registration order.
type registry struct {
	stateChanges []func(Deps) (StateChange, error)
	automations  []func(Deps) (Automation, error)
}

// registered is the registry the slices add themselves to from init.
var registered registry

// RegisterStateChange adds a state-change slice factory. Slices call it from
// init; Build calls the factory.
func RegisterStateChange(factory func(Deps) (StateChange, error)) {
	registered.registerStateChange(factory)
}

// RegisterAutomation adds an automation slice factory. Slices call it from
// init; Build calls the factory.
func RegisterAutomation(factory func(Deps) (Automation, error)) {
	registered.registerAutomation(factory)
}

// Build calls every registered factory with d, in registration order within
// each kind, and returns the slices. It stops at the first factory error and
// returns it unwrapped.
func Build(d Deps) (Slices, error) {
	return registered.build(d)
}

func (r *registry) registerStateChange(factory func(Deps) (StateChange, error)) {
	r.stateChanges = append(r.stateChanges, factory)
}

func (r *registry) registerAutomation(factory func(Deps) (Automation, error)) {
	r.automations = append(r.automations, factory)
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
