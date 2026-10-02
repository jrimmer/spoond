// check.go is the one check engine behind every front door (C11):
// POST /hive/check, the CLI and the guide's Next: line all run these
// checks, in the C11 order, against the same Env.
package hive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Status is a check's outcome.
type Status string

const (
	// Pass means the check succeeded.
	Pass Status = "pass"
	// Fail means the check ran and the project does not satisfy it.
	Fail Status = "fail"
	// Skip means the check did not run: either an earlier check it
	// depends on failed, or the Env it ran against cannot do it.
	Skip Status = "skip"
)

// Result is one check's outcome.
type Result struct {
	Status Status
	// Detail says what happened, in one line.
	Detail string
	// Remedy is the exact fix when the check failed.
	Remedy string
}

// Env holds every side effect the checks need: looking up the image
// catalog, building the worker image, git against the project's repo,
// the trial lease and the budget lookup. The CLI and the tests run it
// against a fake or a limited implementation; the server (step 3) runs
// it against the real thing.
type Env interface {
	// Facts returns the instance facts the derivations use.
	Facts(ctx context.Context) (Instance, error)
	// ListImages returns the image catalog's names.
	ListImages(ctx context.Context) ([]string, error)
	// BuildImage builds the named worker image.
	BuildImage(ctx context.Context, image string) error
	// PushScratch clones repo with the deploy key, pushes a scratch
	// branch and deletes it again.
	PushScratch(ctx context.Context, repo string) error
	// Reachable runs a trial lease with the derived allowlist and
	// reports whether it reaches every needs: target.
	Reachable(ctx context.Context, allowlist []string, needs []string) error
	// RunGate runs one gate on the repo's default branch.
	RunGate(ctx context.Context, repo, gate string) error
	// Budget looks up the project's bee-hour budget.
	Budget(ctx context.Context, project string) (Budget, error)
}

// Check is one named step of the enlistment check.
type Check struct {
	Name string
	Run  func(ctx context.Context, p *Project, env Env) Result
}

// failf returns a Fail result with a detail and a remedy.
func failf(detail, remedy string) Result {
	return Result{Status: Fail, Detail: detail, Remedy: remedy}
}

// resultOf turns an Env outcome into a Result: nil is a Pass with the
// given detail, a *SkipError is a Skip carrying the reason, and
// anything else is a Fail with the given remedy.
func resultOf(err error, detail, remedy string) Result {
	if err == nil {
		return Result{Status: Pass, Detail: detail}
	}
	var skip *SkipError
	if errors.As(err, &skip) {
		return Result{Status: Skip, Detail: skip.Reason}
	}
	return failf(err.Error(), remedy)
}

// Check names, used by dependency skips and by the guide (C11). The
// order of checks is the C11 order.
const (
	CheckSchema = "hive.yaml"
	CheckImage  = "base image"
	CheckBuild  = "worker image"
	CheckKey    = "deploy key"
	CheckLease  = "trial lease"
	CheckGates  = "gates"
	CheckBudget = "budget"
)

// CheckNames lists the checks in C11 order.
var CheckNames = []string{
	CheckSchema, CheckImage, CheckBuild, CheckKey, CheckLease, CheckGates, CheckBudget,
}

// Checks is the check list in C11 order: hive.yaml parses and
// validates; the base image exists; the worker image builds; the deploy
// key can clone and push a scratch branch; a trial lease with the
// derived allowlist reaches every needs: target; the gates pass on the
// default branch; a budget is set.
//
// A failed check makes every later check that depends on it Skip with
// Detail "needs <check>", so a report always shows where the chain
// broke and what to fix first.
func Checks() []Check {
	return []Check{
		{
			Name: CheckSchema,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				problems := p.Validate()
				if len(problems) == 0 {
					return Result{Status: Pass, Detail: "parses and validates"}
				}
				lines := make([]string, 0, len(problems))
				for _, pr := range problems {
					lines = append(lines, pr.String())
				}
				return failf(strings.Join(lines, "; "), problems[0].Remedy)
			},
		},
		{
			Name: CheckImage,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				names, err := env.ListImages(ctx)
				if err != nil {
					var skip *SkipError
					if errors.As(err, &skip) {
						return Result{Status: Skip, Detail: skip.Reason}
					}
					return failf(fmt.Sprintf("list images: %v", err),
						"point SPOOND_API at the lease API and set SPOOND_TOKEN to a valid consumer token, then run the check again.")
				}
				for _, n := range names {
					if n == p.BaseImage {
						return Result{Status: Pass, Detail: p.BaseImage}
					}
				}
				sort.Strings(names)
				return failf(fmt.Sprintf("%s is not in the catalog (have: %s)", p.BaseImage, strings.Join(names, ", ")),
					fmt.Sprintf("set base_image to an image in the catalog: %s.", strings.Join(names, ", ")))
			},
		},
		{
			Name: CheckBuild,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				worker := p.WorkerImage()
				return resultOf(env.BuildImage(ctx, worker),
					worker+" builds",
					fmt.Sprintf("build the worker image on the host: spoond images build %s.", p.BaseImage))
			},
		},
		{
			Name: CheckKey,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				return resultOf(env.PushScratch(ctx, p.Repo),
					"clones, pushes and deletes a scratch branch",
					"authorize the project's deploy key on the repository with write access, then run the check again.")
			},
		},
		{
			Name: CheckLease,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				inst, err := env.Facts(ctx)
				if err != nil {
					return failf(fmt.Sprintf("instance facts: %v", err),
						"ask the owner to fix the instance's guide, then run the check again.")
				}
				allowlist := p.Allowlist(inst)
				return resultOf(env.Reachable(ctx, allowlist, p.Needs),
					leaseDetail(p.Needs, allowlist),
					fmt.Sprintf("drop the needs: entry that a trial lease cannot reach, or ask the owner to fix the target (needs: %s).", strings.Join(p.Needs, ", ")))
			},
		},
		{
			Name: CheckGates,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				for _, gate := range p.Gates {
					g := gate
					if err := env.RunGate(ctx, p.Repo, g); err != nil {
						var skip *SkipError
						if errors.As(err, &skip) {
							return Result{Status: Skip, Detail: skip.Reason}
						}
						return failf(fmt.Sprintf("%s: %v", g, err),
							fmt.Sprintf("make %s pass on the default branch, or drop it from gates.", g))
					}
				}
				return Result{Status: Pass, Detail: gatesDetail(p.Gates)}
			},
		},
		{
			Name: CheckBudget,
			Run: func(ctx context.Context, p *Project, env Env) Result {
				b, err := env.Budget(ctx, p.Project)
				if err != nil {
					var skip *SkipError
					if errors.As(err, &skip) {
						return Result{Status: Skip, Detail: skip.Reason}
					}
					return failf(fmt.Sprintf("look up budget: %v", err),
						"set a bee-hour budget for the project with the owner, then run the check again.")
				}
				if !b.Set {
					return failf("no budget is set",
						fmt.Sprintf("set a bee-hour budget for %s with the owner (POST /hive/projects).", p.Project))
				}
				return Result{Status: Pass, Detail: fmt.Sprintf("%d bee-hours/day", b.HoursPerDay)}
			},
		},
	}
}

// leaseDetail describes the trial lease check's pass: which needs:
// targets it reached and how wide the derived allowlist was.
func leaseDetail(needs, allowlist []string) string {
	what := "none declared"
	if len(needs) > 0 {
		what = strings.Join(needs, ", ")
	}
	return fmt.Sprintf("trial lease reached every needs: target (%s) with %d allowlisted host(s)", what, len(allowlist))
}

// gatesDetail describes the gates check's pass.
func gatesDetail(gates []string) string {
	if len(gates) == 1 {
		return "1 gate passes on the default branch"
	}
	return fmt.Sprintf("%d gates pass on the default branch", len(gates))
}

// lineEntry is one line of a Report.
type lineEntry struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// Report is the outcome of running every check. It renders as text
// (one line per check) or as JSON, and in both ends with the next step:
// the first failing check's remedy, or the enlistment route when
// nothing failed.
type Report struct {
	entries []lineEntry
	next    string
	// failed reports whether any check failed.
	failed bool
}

// NextEnlist is the Next: line of a report with no failing check.
const NextEnlist = "enlist: POST /hive/projects"

// Lines returns one entry per check, in run order.
func (r Report) Lines() []lineEntry {
	if r.entries == nil {
		return nil
	}
	return append([]lineEntry(nil), r.entries...)
}

// Next returns the first failing check's remedy, or the enlistment
// route when nothing failed.
func (r Report) Next() string { return r.next }

// OK reports whether no check failed: everything passed or was skipped.
func (r Report) OK() bool { return !r.failed }

// firstRemedy returns the remedy of the first failing entry, or the
// enlistment route when nothing failed.
func (r Report) firstRemedy() string {
	for _, e := range r.entries {
		if e.Status == Fail {
			return e.Remedy
		}
	}
	return NextEnlist
}

// Run runs the checks in C11 order and returns the report. A failed
// check makes every later check that depends on it Skip with Detail
// "needs <check>", so exactly one remedy — the first — is ever worth
// reading.
func Run(ctx context.Context, p *Project, env Env) Report {
	rep := Report{next: NextEnlist}
	failed := map[string]bool{}
	for _, c := range Checks() {
		if blocked, ok := dependency(c.Name, failed); ok {
			rep.entries = append(rep.entries, lineEntry{
				Name:   c.Name,
				Status: Skip,
				Detail: "needs " + blocked,
			})
			continue
		}
		res := c.Run(ctx, p, env)
		if res.Status == Fail {
			rep.failed = true
			failed[c.Name] = true
		}
		rep.entries = append(rep.entries, lineEntry{
			Name:   c.Name,
			Status: res.Status,
			Detail: res.Detail,
			Remedy: res.Remedy,
		})
	}
	rep.next = rep.firstRemedy()
	return rep
}

// dependency reports the failed check that blocks name, if any. After
// the first failure every later check depends on it: the chain is
// sequential, and none of them can run without everything before it.
func dependency(name string, failed map[string]bool) (string, bool) {
	for _, n := range CheckNames {
		if n == name {
			return "", false
		}
		if failed[n] {
			return n, true
		}
	}
	return "", false
}

// String renders the report as text: one line per check
// (✓ name, ✗ name: detail followed by an indented "fix: remedy",
// - name: skipped (detail)), ending with exactly one Next: line.
func (r Report) String() string {
	var b strings.Builder
	for _, e := range r.entries {
		switch e.Status {
		case Pass:
			fmt.Fprintf(&b, "✓ %s\n", e.Name)
		case Fail:
			fmt.Fprintf(&b, "✗ %s: %s\n", e.Name, e.Detail)
			fmt.Fprintf(&b, "  fix: %s\n", e.Remedy)
		case Skip:
			fmt.Fprintf(&b, "- %s: skipped (%s)\n", e.Name, e.Detail)
		}
	}
	fmt.Fprintf(&b, "Next: %s\n", r.next)
	return b.String()
}

// MarshalJSON renders the report with one entry per check and a
// top-level "next" — the same content as the text form.
func (r Report) MarshalJSON() ([]byte, error) {
	out := struct {
		Checks []lineEntry `json:"checks"`
		Next   string      `json:"next"`
		OK     bool        `json:"ok"`
	}{Checks: r.entries, Next: r.next, OK: !r.failed}
	if out.Checks == nil {
		out.Checks = []lineEntry{}
	}
	return json.Marshal(out)
}
