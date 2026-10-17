// Ordered phases for `spoond install`: it runs all of them in table
// order, `spoond install <phase>` runs the one named. Only check is
// implemented in this slice; the later slices fill the other rows
// (docs/plans/2026-10-16-4.0-install.md, work slice S2).
package install

import (
	"context"
	"errors"
	"fmt"
)

// Phase names, in run order: each depends on the ones before it.
const (
	PhaseCheck   = "check"
	PhaseHost    = "host"
	PhaseRuntime = "runtime"
	PhaseSpoond  = "spoond"
	PhaseImages  = "images"
	PhaseDoctor  = "doctor"
	errNotYetFmt = "%s: not implemented yet (4.0 install slice pending)"
)

// ErrNotImplemented is what the phases after check return until their
// slice lands; `spoond install` with no phase argument stops on it.
var ErrNotImplemented = errors.New("not implemented yet")

// ErrBadPhase is returned for a phase name that is not in the table.
var ErrBadPhase = errors.New("unknown phase")

// ErrNotRoot is returned when the real host is touched without root.
// The installer writes /etc, /lib and /opt, so production runs are
// root; tests are unaffected (they set a temp Root).
var ErrNotRoot = errors.New("spoond install must run as root (it writes /etc, /opt, /lib and /var/lib)")

// errNotYet is the placeholder the unimplemented phases return.
func errNotYet(name string) error {
	return fmt.Errorf("%s: %w (4.0 install slice pending)", name, ErrNotImplemented)
}

// Order is the phase table in run order.
var Order = []string{PhaseCheck, PhaseHost, PhaseRuntime, PhaseSpoond, PhaseImages, PhaseDoctor}

// Phases maps a phase name to its implementation. Phase rows not yet
// written return errNotYet so later slices fill them one by one.
var Phases = map[string]Phase{
	PhaseCheck:   checkPhase,
	PhaseHost:    func(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet(PhaseHost) },
	PhaseRuntime: func(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet(PhaseRuntime) },
	PhaseSpoond:  func(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet(PhaseSpoond) },
	PhaseImages:  func(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet(PhaseImages) },
	PhaseDoctor:  func(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet(PhaseDoctor) },
}

// RunAll runs the named phases in table order and stops at the first
// failure; with no names it runs every phase. It returns the rows of
// the phases that ran and the error that stopped it.
func RunAll(ctx context.Context, env *Env, names ...string) ([]Item, error) {
	if len(names) == 0 {
		names = Order
	}
	var all []Item
	for _, name := range names {
		fn, ok := Phases[name]
		if !ok {
			return all, fmt.Errorf("%w: %q (want one of check, host, runtime, spoond, images, doctor)", ErrBadPhase, name)
		}
		items, err := RunPhase(ctx, env, name, fn)
		all = append(all, items...)
		if err != nil {
			return all, err
		}
	}
	return all, nil
}
