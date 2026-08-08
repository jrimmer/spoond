package runner

import "testing"

func TestEvalOrFallbackUsesVarWhenSet(t *testing.T) {
	c := &EvalContext{
		Vars: map[string]string{"LLM_PROVIDER": "openai-compatible"},
	}
	got := c.Eval(`${{ vars.LLM_PROVIDER || 'anthropic' }}`)
	if got != "openai-compatible" {
		t.Fatalf("got %q, want openai-compatible", got)
	}
}

func TestEvalOrFallbackUsesDefaultWhenVarEmpty(t *testing.T) {
	c := &EvalContext{Vars: map[string]string{}}
	got := c.Eval(`${{ vars.LLM_PROVIDER || 'openai-compatible' }}`)
	if got != "openai-compatible" {
		t.Fatalf("got %q, want openai-compatible", got)
	}
}

func TestEvalOrFallbackChained(t *testing.T) {
	c := &EvalContext{Vars: map[string]string{}}
	got := c.Eval(`${{ vars.A || vars.B || 'fallback' }}`)
	if got != "fallback" {
		t.Fatalf("got %q, want fallback", got)
	}
}

func TestEvalOrFallbackPreservesQuotedDefault(t *testing.T) {
	c := &EvalContext{Vars: map[string]string{}}
	got := c.Eval(`${{ vars.LLM_ENDPOINT || 'https://opencode.ai/zen/v1/chat/completions' }}`)
	if got != "https://opencode.ai/zen/v1/chat/completions" {
		t.Fatalf("got %q, want the URL", got)
	}
}

func TestEvalOrFallbackDoesNotSplitInsideQuotes(t *testing.T) {
	c := &EvalContext{Vars: map[string]string{}}
	// The default contains no ||, but ensure a quoted value with a pipe
	// isn't split.
	got := c.Eval(`${{ vars.X || 'a|b' }}`)
	if got != "a|b" {
		t.Fatalf("got %q, want a|b", got)
	}
}

func TestEvalPlainLookupStillWorks(t *testing.T) {
	c := &EvalContext{
		GitHub: map[string]string{"repository": "dbl/site"},
	}
	got := c.Eval(`${{ github.repository }}`)
	if got != "dbl/site" {
		t.Fatalf("got %q, want dbl/site", got)
	}
}
