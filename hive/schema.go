// schema.go is the one field table and the one needs: table behind both
// validation and the guide (C11): Validate walks the field table, Allowlist
// reads the needs: table, and the server's GET /hive/guide renders both, so
// the rules a hive.yaml must satisfy and the rules the instance teaches can
// never disagree.
package hive

import (
	"fmt"
	"strconv"
	"strings"
)

// required is what the guide shows for a field that must be present.
const required = "(required)"

// Field is one field of Project as the validator and the guide see it:
// its YAML key, its type, its default and the rule it must satisfy.
type Field struct {
	// Name is the YAML key.
	Name string `json:"name"`
	// Type is the value's shape.
	Type string `json:"type"`
	// Default is what an absent field becomes, or "(required)".
	Default string `json:"default"`
	// Rule is the one-line constraint Validate enforces.
	Rule string `json:"rule"`
	// check returns the field's problems; nil when the field carries no
	// rule of its own.
	check func(p *Project) []Problem
}

// Fields lists every field of Project, in the order a hive.yaml lists
// them. Validate walks this table and the guide renders it, so a field
// cannot exist without both.
func Fields() []Field {
	return []Field{
		{
			Name:    "project",
			Type:    "string",
			Default: required,
			Rule: fmt.Sprintf("a lowercase name of 2-31 characters from [a-z0-9-] starting with a letter, matching %s",
				projectNameRE.String()),
			check: func(p *Project) []Problem {
				if projectNameRE.MatchString(p.Project) {
					return nil
				}
				return []Problem{{
					Field: "project",
					Remedy: fmt.Sprintf("rename the project to a lowercase name of 2-31 characters from [a-z0-9-] starting with a letter, matching %s.",
						projectNameRE.String()),
				}}
			},
		},
		{
			Name:    "repo",
			Type:    "string",
			Default: required,
			Rule:    "an ssh:// or https:// git URL with a host; the host is always on the derived allowlist",
			check: func(p *Project) []Problem {
				if host, err := repoHost(p.Repo); err == nil && host != "" {
					return nil
				}
				return []Problem{{
					Field:  "repo",
					Remedy: "set repo to an ssh:// or https:// git URL with a host, e.g. ssh://git@git.lacy.casa/lacy.casa/hrmny.git.",
				}}
			},
		},
		{
			Name:    "base_image",
			Type:    "string",
			Default: required,
			Rule:    "an image this instance has a current build of; the worker image is derived from it as <base>-worker",
			check: func(p *Project) []Problem {
				if p.BaseImage != "" {
					return nil
				}
				return []Problem{{
					Field:  "base_image",
					Remedy: "set base_image to an image this instance has a current build of (GET /api/images lists them).",
				}}
			},
		},
		{
			Name:    "gates",
			Type:    "[]string",
			Default: required,
			Rule:    "at least one command that says what done means; the bee and the verifier both run them",
			check: func(p *Project) []Problem {
				if len(p.Gates) > 0 {
					return nil
				}
				return []Problem{{
					Field:  "gates",
					Remedy: "list at least one gate command under gates that says what done means for this project, e.g. mix test.",
				}}
			},
		},
		{
			Name:    "needs",
			Type:    "[]string",
			Default: "[]",
			Rule: "any of the known keys (" + strings.Join(KnownNeeds, ", ") +
				"); each adds its fixed target to the derived allowlist, and no raw address is written in the file",
			check: func(p *Project) []Problem {
				var problems []Problem
				for _, n := range p.Needs {
					if isKnownNeed(n) {
						continue
					}
					problems = append(problems, Problem{
						Field:  "needs",
						Remedy: fmt.Sprintf("replace the unknown entry %q with one of the known keys (%s), or drop it.", n, strings.Join(KnownNeeds, ", ")),
					})
				}
				return problems
			},
		},
		{
			Name:    "max_workers",
			Type:    "int",
			Default: strconv.Itoa(DefaultMaxWorkers),
			Rule:    fmt.Sprintf("an integer between %d and %d", MinMaxWorkers, MaxMaxWorkers),
			check: func(p *Project) []Problem {
				if p.MaxWorkers >= MinMaxWorkers && p.MaxWorkers <= MaxMaxWorkers {
					return nil
				}
				return []Problem{{
					Field:  "max_workers",
					Remedy: fmt.Sprintf("set max_workers to an integer between %d and %d.", MinMaxWorkers, MaxMaxWorkers),
				}}
			},
		},
		{
			Name:    "models",
			Type:    "map of string",
			Default: fmt.Sprintf("{implement: %s, verify: %s}", DefaultModel, DefaultModel),
			Rule:    "Bifrost model names for the implement and verify passes; either may be set on its own",
		},
	}
}

// Need is one needs: key: what it adds to the derived allowlist (C10).
// This table is the one mapping behind KnownNeeds, Allowlist, the trial
// lease's probe targets and the guide's needs section.
type Need struct {
	// Key is the accepted needs: entry.
	Key string `json:"key"`
	// Adds says, in one line, what the key lets a worker reach.
	Adds string `json:"adds"`
	// target is the fixed network target the key maps to on an instance.
	target func(Instance) string
}

// needsTable lists every known needs: key with its fixed target.
var needsTable = []Need{
	{
		Key:  NeedLeases,
		Adds: "the lease API, so a worker can grant and release its own leases",
		target: func(i Instance) string {
			return i.LeaseAPI
		},
	},
	{
		Key:  NeedRegistry,
		Adds: "the image registry, so a worker can pull and push images",
		target: func(i Instance) string {
			return i.Registry
		},
	},
}

// Needs lists every accepted needs: key with what it adds.
func Needs() []Need {
	return append([]Need(nil), needsTable...)
}

// NeedTarget returns the fixed target key maps to on inst, or "" when the
// key is unknown.
func NeedTarget(key string, inst Instance) string {
	for _, n := range needsTable {
		if n.Key == key {
			return n.target(inst)
		}
	}
	return ""
}

// NeedTargets maps every known needs: entry to its fixed target on inst,
// in file order, dropping unknown keys (Validate reports those).
func NeedTargets(needs []string, inst Instance) []string {
	var out []string
	for _, n := range needs {
		if t := NeedTarget(n, inst); t != "" {
			out = append(out, t)
		}
	}
	return out
}
