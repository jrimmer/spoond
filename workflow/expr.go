package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// ExprContext provides the variables available during expression evaluation.
type ExprContext struct {
	GitHub  map[string]string
	Secrets map[string]string
	Vars    map[string]string
	Env     map[string]string
}

// EvalExpr evaluates a ${{ }} expression against the given context.
// This is a minimal implementation supporting dot access, string literals,
// and basic comparison/boolean operators.
func EvalExpr(expr string, ctx *ExprContext) (string, error) {
	expr = strings.TrimSpace(expr)
	// Strip ${{ }} wrapper if present
	if strings.HasPrefix(expr, "${{") && strings.HasSuffix(expr, "}}") {
		expr = strings.TrimSpace(expr[3 : len(expr)-2])
	}

	val, err := evalInner(expr, ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", val), nil
}

var exprRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// ExpandExpressions replaces all ${{ }} occurrences in a string.
func ExpandExpressions(s string, ctx *ExprContext) (string, error) {
	var err error
	result := exprRe.ReplaceAllStringFunc(s, func(match string) string {
		if err != nil {
			return match
		}
		val, e := EvalExpr(match, ctx)
		if e != nil {
			err = e
			return match
		}
		return val
	})
	return result, err
}

// evalInner handles a single expression (without ${{ }}).
func evalInner(expr string, ctx *ExprContext) (interface{}, error) {
	// TODO: full recursive-descent parser for Phase 1
	// For now, handle simple dot access: github.sha, secrets.X, env.Y, vars.Z
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, ") && strings.HasSuffix(expr, ") {
		return expr[1 : len(expr)-1], nil
	}

	parts := strings.SplitN(expr, ".", 2)
	if len(parts) == 2 {
		ns, key := parts[0], parts[1]
		var src map[string]string
		switch ns {
		case "github":
			src = ctx.GitHub
		case "secrets":
			src = ctx.Secrets
		case "vars":
			src = ctx.Vars
		case "env":
			src = ctx.Env
		}
		if src != nil {
			if v, ok := src[key]; ok {
				return v, nil
			}
			return "", nil
		}
	}

	return nil, fmt.Errorf("unsupported expression: %q", expr)
}
