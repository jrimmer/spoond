// Ordered phases for `spoond install`: it runs all of them in table
// order, `spoond install <phase>` runs the one named. Only check is
// implemented in this slice; the later slices fill the other rows
// (docs/plans/2026-10-16-4.0-install.md, work slice S2).
package install

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Phase names, in run order: each depends on the ones before it.
const (
	PhaseCheck   = "check"
	PhaseHost    = "host"
	PhaseRuntime = "runtime"
	PhaseSpoond  = "spoond"
	PhaseImages  = "images"
	PhaseDoctor  = "doctor"
)

// ErrNotImplemented is what the phases after check return until their
// slice lands; the CLI prints a hint with its `install` usage so the
// operator knows the phase exists but this binary is from an earlier
// slice.
var ErrNotImplemented = errors.New("not implemented yet")

// ErrBadPhase is returned for a phase name that is not in the table.
var ErrBadPhase = errors.New("unknown phase")

// ErrNotRoot is returned when the real host is touched without root.
// The installer writes /etc, /lib and /opt, so production runs are
// root; tests are unaffected (they set a temp Root).
var ErrNotRoot = errors.New("spoond install must run as root (it writes /etc, /opt, /lib and /var/lib)")

// errNotYet is the placeholder the unimplemented phases return:
// the fix for the operator (this phase is not in this slice yet),
// without the phase-name prefix RunPhase adds on top.
func errNotYet() error {
	return fmt.Errorf("%w (4.0 install slice pending)", ErrNotImplemented)
}

// Order is the phase table in run order.
var Order = []string{PhaseCheck, PhaseHost, PhaseRuntime, PhaseSpoond, PhaseImages, PhaseDoctor}

// Phases maps a phase name to its implementation. Every row starts as
// a placeholder returning errNotYet; each phase's own file replaces its
// row from an init function (check.go: Phases[PhaseCheck] = checkPhase),
// so the phases land independently without touching this table.
var Phases = map[string]Phase{
	PhaseCheck:   notYet,
	PhaseHost:    notYet,
	PhaseRuntime: notYet,
	PhaseSpoond:  notYet,
	PhaseImages:  notYet,
	PhaseDoctor:  notYet,
}

// notYet is the placeholder phase.
func notYet(ctx context.Context, env *Env) ([]Item, error) { return nil, errNotYet() }

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
			return all, fmt.Errorf("%w: %q (want one of %s)", ErrBadPhase, name, strings.Join(Order, ", "))
		}
		items, err := RunPhase(ctx, env, name, fn)
		all = append(all, items...)
		if err != nil {
			return all, err
		}
	}
	return all, nil
}
