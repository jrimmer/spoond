package runner

import (
	"regexp"
	"strings"
)

// exprRe matches a ${{ ... }} expression.
var exprRe = regexp.MustCompile(`\$\{\{\s*([^}]+?)\s*\}\}`)

// EvalContext holds the values available to expression evaluation.
type EvalContext struct {
	// GitHub holds the github context (e.g. github.repository).
	GitHub map[string]string
	// Env holds the env context.
	Env map[string]string
	// Secrets holds the secrets context.
	Secrets map[string]string
	// Vars holds the vars context.
	Vars map[string]string
	// Steps holds step outputs keyed by step id.
	Steps map[string]map[string]string
}

// Eval replaces all ${{ }} expressions in s with their evaluated
// values. Unknown expressions evaluate to the empty string.
func (c *EvalContext) Eval(s string) string {
	return exprRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := exprRe.FindStringSubmatch(m)[1]
		return c.lookup(strings.TrimSpace(inner))
	})
}

// lookup resolves a single expression body like "github.repository" or
// "env.FOO" or "steps.build.outputs.id".
func (c *EvalContext) lookup(expr string) string {
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return ""
	}
	switch parts[0] {
	case "github":
		return c.GitHub[strings.Join(parts[1:], ".")]
	case "env":
		return c.Env[strings.Join(parts[1:], ".")]
	case "secrets":
		return c.Secrets[strings.Join(parts[1:], ".")]
	case "vars":
		return c.Vars[strings.Join(parts[1:], ".")]
	case "steps":
		// steps.<id>.outputs.<name>
		if len(parts) >= 4 && parts[2] == "outputs" {
			if step, ok := c.Steps[parts[1]]; ok {
				return step[parts[3]]
			}
		}
		return ""
	}
	return ""
}
