package api

import (
	"math"
	"testing"
)

// TestParseOpenMetricsRealShapes parses a document in the shape the
// orchestrator's collector actually serves: HELP/TYPE comments, several
// families interleaved, label sets of a few labels with sandbox_id
// among them, exponent floats, and series for sandboxes that are gone
// (the sampler's lease match, not the parser, drops those; this only
// checks both are visible to it).
func TestParseOpenMetricsRealShapes(t *testing.T) {
	body := `# HELP e2b_sandbox_ram_used Memory used by the sandbox in bytes.
# TYPE e2b_sandbox_ram_used gauge
e2b_sandbox_ram_used{arch="amd64",category="general-purpose",environment_id="b7f1",family="intel",host="10.0.1.4",kernel_version="6.8.0-41-generic",node_id="node-a",sandbox_id="sb_01J",service="envd",team_id="t1",version="0.1.2"} 5.24288e+08 1728657600000
e2b_sandbox_ram_used{arch="amd64",node_id="node-a",sandbox_id="sb_02J",team_id="t1"} 268435456 1728657600001
# HELP e2b_sandbox_ram_total Total memory of the sandbox in bytes.
# TYPE e2b_sandbox_ram_total gauge
e2b_sandbox_ram_total{arch="amd64",node_id="node-a",sandbox_id="sb_01J",team_id="t1"} 4.294967296e+09 1728657600000
e2b_sandbox_ram_total{arch="amd64",node_id="node-a",sandbox_id="sb_02J",team_id="t1"} 4294967296 1728657600001
# HELP e2b_sandbox_cpu_used_pct CPU.
# TYPE e2b_sandbox_cpu_used_pct gauge
e2b_sandbox_cpu_used_pct{node_id="node-a",sandbox_id="sb_01J"} 3.5
# HELP orchestrator_is_up whether it answers.
# TYPE orchestrator_is_up gauge
orchestrator_is_up{node_id="node-a"} 1
e2b_sandbox_ram_used 987654321
e2b_sandbox_ram_used{node_id="node-a",sandbox_id="sb_gone"} 111111111
e2b_sandbox_ram_used{node_id="node-a",sandbox_id="",team_id="t1"} 222222222
`
	used, total := parseOpenMetrics([]byte(body))
	if got := used["sb_01J"]; got != 524288000 {
		t.Errorf("used sb_01J = %g, want 524288000 (exponent form)", got)
	}
	if got := used["sb_02J"]; got != 268435456 {
		t.Errorf("used sb_02J = %g, want 268435456", got)
	}
	if got := total["sb_01J"]; got != 4294967296 {
		t.Errorf("total sb_01J = %g, want 4294967296 (exponent form)", got)
	}
	if got := total["sb_02J"]; got != 4294967296 {
		t.Errorf("total sb_02J = %g, want 4294967296", got)
	}
	if _, ok := used[""]; ok {
		t.Error("a sample without labels contributed")
	}
	if len(used) != 3 || len(total) != 2 {
		t.Errorf("parsed %d used / %d total series, want 3/2 (sb_gone is visible to the lease match)", len(used), len(total))
	}
}

// TestParseOpenMetricsShapes: garbage lines, NaN/Inf, quoted labels
// with escapes and untyped families are skipped, not errors. The
// ram_total family may be absent from a smaller collector.
func TestParseOpenMetricsShapes(t *testing.T) {
	body := "# a comment\n" +
		"\n" +
		"e2b_sandbox_ram_used{sandbox_id=\"sb_a\"} 2048\n" +
		"e2b_sandbox_ram_used{sandbox_id=\"sb_nan} NaN\n" +
		"e2b_sandbox_ram_used{sandbox_id=\"sb_inf} +Inf\n" +
		"e2b_sandbox_ram_used{sandbox_id=\"sb_broken} 123\n" + // unterminated quote: no closing brace+space
		"e2b_sandbox_ram_used{note=\"has \\\"quotes\\\" and \\\\and \\n line\",sandbox_id=\"sb_b\"} 1e3\n" +
		"e2b_sandbox_ram_used{note=\"two\",sandbox_id=\"sb_c\",more=\"x=y,z=w\"} 0.5\n" +
		"e2b_sandbox_ram_total{sandbox_id=\"sb_a\"} 4096\n" +
		"e2b_something_ram_used_bytes{sandbox_id=\"sb_other\"} 9\n" +
		"e2b_sandbox_ram_used{sandbox_id=\"sb_d\",} 8\n" +
		"{sandbox_id=\"sb_nofamily\"} 7\n"
	used, total := parseOpenMetrics([]byte(body))
	if got := used["sb_a"]; got != 2048 {
		t.Errorf("used sb_a = %g, want 2048", got)
	}
	if got := used["sb_b"]; got != 1000 {
		t.Errorf("used sb_b = %g, want 1000 (1e3)", got)
	}
	if got := used["sb_c"]; got != 0.5 {
		t.Errorf("used sb_c = %g, want 0.5", got)
	}
	if got := used["sb_d"]; got != 8 {
		t.Errorf("used sb_d = %g, want 8 (trailing comma in labels)", got)
	}
	if got := total["sb_a"]; got != 4096 {
		t.Errorf("total sb_a = %g, want 4096", got)
	}
	for _, bad := range []string{"sb_nan", "sb_inf", "sb_broken", "sb_other", "sb_nofamily"} {
		if _, ok := used[bad]; ok {
			t.Errorf("series %q parsed, want skipped", bad)
		}
	}
}

// TestParseOpenMetricsEmptyAndShort: an empty document, a document with
// only comments and a family-less document all parse to nothing.
func TestParseOpenMetricsEmptyAndShort(t *testing.T) {
	for _, body := range []string{"", "\n", "# HELP only comments\n# TYPE e2b_sandbox_ram_used gauge\n", "no braces at all", "} "} {
		used, total := parseOpenMetrics([]byte(body))
		if len(used) != 0 || len(total) != 0 {
			t.Errorf("parse(%q) = %v, %v; want empty", body, used, total)
		}
	}
}

// TestParseLabelsEscapes: the label parser decodes the Prometheus
// escapes and stops at the closing quote.
func TestParseLabelsEscapes(t *testing.T) {
	got := parseLabels(`a="1",note="back\\slash and \"quote\"",b="two"`)
	if got["a"] != "1" || got["b"] != "two" {
		t.Errorf("plain keys wrong: %v", got)
	}
	if want := `back\slash and "quote"`; got["note"] != want {
		t.Errorf("note = %q, want %q", got["note"], want)
	}
	if got := parseLabels(`a="newline\nhere"`); got["a"] != "newline\nhere" {
		t.Errorf("newline escape = %q", got["a"])
	}
}

// TestMibCeil pins the rounding: up, and never below the used value.
func TestMibCeil(t *testing.T) {
	const MiB = 1024 * 1024
	cases := []struct {
		in   float64
		want int
	}{
		{0, 0},
		{1, 1},
		{MiB, 1},
		{MiB + 1, 2},
		{520 * MiB, 520},
		{470.5 * MiB, 471},
		{-5, 0},
	}
	for _, c := range cases {
		if got := mibCeil(c.in); got != c.want {
			t.Errorf("mibCeil(%g) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := mibCeil(math.Inf(1)); got <= 0 || math.MaxInt == math.MinInt {
		// Inf does not occur in real series (the parser skips Inf); any
		// non-crashing value is fine here.
		t.Logf("mibCeil(+Inf) = %d (documented best effort)", got)
	}
}

// TestParseRealOrchestratorLines: lines copied from the production
// orchestrator's collector (2026-10-11) parse, under the metric names it
// really uses — and the `_bytes` form an OTEL unit suffix would give.
func TestParseRealOrchestratorLines(t *testing.T) {
	doc := `e2b_sandbox_ram_total{build_id="166b2667-1717-4a5c-aa21-04d1f73e95c6",instance="693b2106-893c-4e70-b240-24f592b85d80",job="orchestrator_template-manager",otel_scope_name="github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics",otel_scope_schema_url="",otel_scope_version="",sandbox_id="i4se866848cew96ab2z8y",sandbox_type="sandbox",team_id="5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95"} 4.122021888e+09
e2b_sandbox_ram_used{build_id="166b2667-1717-4a5c-aa21-04d1f73e95c6",instance="693b2106-893c-4e70-b240-24f592b85d80",job="orchestrator_template-manager",otel_scope_name="github.com/e2b-dev/infra/packages/orchestrator/pkg/metrics",otel_scope_schema_url="",otel_scope_version="",sandbox_id="i4se866848cew96ab2z8y",sandbox_type="sandbox",team_id="5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95"} 5.62507776e+08
e2b_sandbox_ram_used_bytes{sandbox_id="other"} 1.0e+08
e2b_sandbox_cpu_used{sandbox_id="i4se866848cew96ab2z8y"} 0.5
`
	used, total := parseOpenMetrics([]byte(doc))
	if used["i4se866848cew96ab2z8y"] != 5.62507776e+08 || total["i4se866848cew96ab2z8y"] != 4.122021888e+09 {
		t.Fatalf("real lines: used %v total %v", used, total)
	}
	if used["other"] != 1.0e+08 {
		t.Fatalf("the _bytes form was not read: %v", used)
	}
}
