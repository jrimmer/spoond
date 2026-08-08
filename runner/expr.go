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
		return c.evalExpr(strings.TrimSpace(inner))
	})
}

// evalExpr resolves a single expression body, supporting the GitHub
// Actions `||` fallback operator: `${{ vars.X || 'default' }}` returns
// the first non-empty operand.
func (c *EvalContext) evalExpr(expr string) string {
	// Split on top-level `||` (not inside quotes).
	parts := splitTopLevelOr(expr)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Strip surrounding quotes for string literals.
		if len(part) >= 2 && (part[0] == '\'' || part[0] == '"') && part[len(part)-1] == part[0] {
			part = part[1 : len(part)-1]
		}
		// If it's a lookup expression, resolve it. Only treat known
		// context prefixes (github./env./secrets./vars./steps.) as
		// lookups — a bare string like a URL with dots must not be
		// misdetected as one.
		if isLookupExpr(part) {
			if v := c.lookup(part); v != "" {
				return v
			}
			continue
		}
		// Literal (or already-resolved) value.
		if part != "" {
			return part
		}
	}
	return ""
}

// isLookupExpr reports whether an expression body is a context lookup
// (github.*, env.*, secrets.*, vars.*, steps.*) rather than a literal.
func isLookupExpr(expr string) bool {
	for _, prefix := range []string{"github.", "env.", "secrets.", "vars.", "steps."} {
		if strings.HasPrefix(expr, prefix) {
			return true
		}
	}
	return false
}

// splitTopLevelOr splits an expression on `||` operators that are not
// inside single or double quotes.
func splitTopLevelOr(expr string) []string {
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case ch == '|' && i+1 < len(expr) && expr[i+1] == '|' && !inSingle && !inDouble:
			parts = append(parts, cur.String())
			cur.Reset()
			i++ // skip second '|'
			continue
		}
		cur.WriteByte(ch)
	}
	parts = append(parts, cur.String())
	return parts
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
