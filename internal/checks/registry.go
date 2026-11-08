// Package checks holds the evidence checks: a common set that every app gets,
// plus app-specific sets selected by name in apps.yaml (`checks: [kyverno]`).
//
// To add a check set: write constructors returning gate.Check and register
// them in Sets below. A check could also be backed by an external source
// (e.g. a custom resource): implement gate.Check by reading that resource's
// status and mapping it to a gate.Result.
package checks

import (
	"fmt"
	"sort"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
)

// Factory builds fresh check instances for one gate run (checks may keep
// state between polls, so instances are never reused across runs).
type Factory func() []gate.Check

// Common runs for every app.
func Common() []gate.Check {
	return []gate.Check{
		&FluxKustomization{},
		&ChartDigest{},
		&ImageTags{},
		&HelmTests{},
	}
}

// Sets maps the names used in apps.yaml `checks:` to app-specific checks.
var Sets = map[string]Factory{}

// Register adds a check set; called from init() in each set's file.
func Register(name string, f Factory) {
	if _, dup := Sets[name]; dup {
		panic("duplicate check set " + name)
	}
	Sets[name] = f
}

// For returns the full, fresh list of checks for an app.
func For(app config.App) ([]gate.Check, error) {
	out := Common()
	for _, name := range app.Checks {
		f, ok := Sets[name]
		if !ok {
			return nil, fmt.Errorf("app %s: unknown check set %q (known: %v)", app.Name, name, Names())
		}
		out = append(out, f()...)
	}
	return out, nil
}

// Names lists the registered check sets.
func Names() []string {
	var n []string
	for k := range Sets {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
