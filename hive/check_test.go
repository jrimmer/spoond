package hive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// runAll runs the engine over a valid project with the given fake.
func runAll(t *testing.T, f *FakeEnv) Report {
	t.Helper()
	p := validProject()
	return Run(context.Background(), &p, f)
}

// passingFake is a fake where every step succeeds.
func passingFake() *FakeEnv {
	return &FakeEnv{
		Images:     []string{"elixir-release", "go-base"},
		Inst:       testInstance(),
		BudgetResp: Budget{Set: true, HoursPerDay: 24},
	}
}

func TestRunOrder(t *testing.T) {
	f := passingFake()
	rep := runAll(t, f)
	want := []string{"images", "build", "key", "lease", "gate", "gate", "budget"}
	if got := f.Calls(); !equalStrings(got, want) {
		t.Errorf("engine ran %v, want %v", got, want)
	}
	if !rep.OK() {
		t.Errorf("all-passing report is not OK: %s", rep)
	}
	names := make([]string, 0, len(rep.Lines()))
	for _, e := range rep.Lines() {
		names = append(names, e.Name)
	}
	if !equalStrings(names, CheckNames) {
		t.Errorf("check names %v, want the C11 order %v", names, CheckNames)
	}
	if got, want := f.Built(), []string{"elixir-release-worker"}; !equalStrings(got, want) {
		t.Errorf("built %v, want %v", got, want)
	}
	if got := f.GateCommands(); !equalStrings(got, validProject().Gates) {
		t.Errorf("gates run %v, want %v", got, validProject().Gates)
	}
}

// TestRunAllPassText is the golden test of the text report, including
// the Next: line (C11: every answer ends with Next:).
func TestRunAllPassText(t *testing.T) {
	rep := runAll(t, passingFake())
	want := `✓ hive.yaml
✓ base image
✓ worker image
✓ deploy key
✓ trial lease
✓ gates
✓ budget
Next: enlist: POST /hive/projects
`
	if got := rep.String(); got != want {
		t.Errorf("text report:\n%s\nwant:\n%s", got, want)
	}
	if n := strings.Count(rep.String(), "Next: "); n != 1 {
		t.Errorf("report has %d Next: lines, want exactly 1", n)
	}
}

func TestRunSchemaFailSkipsRest(t *testing.T) {
	p := validProject()
	p.BaseImage = "" // schema failure, and nothing after can run
	f := passingFake()
	rep := Run(context.Background(), &p, f)
	if got := f.Calls(); len(got) != 0 {
		t.Errorf("engine ran %v after a schema failure, want nothing", got)
	}
	want := `✗ hive.yaml: base_image: set base_image to an image this instance has a current build of (GET /api/images lists them).
  fix: set base_image to an image this instance has a current build of (GET /api/images lists them).
- base image: skipped (needs hive.yaml)
- worker image: skipped (needs hive.yaml)
- deploy key: skipped (needs hive.yaml)
- trial lease: skipped (needs hive.yaml)
- gates: skipped (needs hive.yaml)
- budget: skipped (needs hive.yaml)
Next: set base_image to an image this instance has a current build of (GET /api/images lists them).
`
	if got := rep.String(); got != want {
		t.Errorf("text report:\n%s\nwant:\n%s", got, want)
	}
	if rep.OK() {
		t.Error("failing report is OK")
	}
	if rep.Next() != "set base_image to an image this instance has a current build of (GET /api/images lists them)." {
		t.Errorf("Next %q", rep.Next())
	}
}

func TestRunImageFailSkipsLaterChecks(t *testing.T) {
	f := passingFake()
	f.Images = []string{"go-base"}
	rep := runAll(t, f)
	lines := rep.Lines()
	if lines[0].Status != Pass || lines[1].Status != Fail {
		t.Fatalf("first two lines %v, %v; want pass then fail", lines[0], lines[1])
	}
	for i, e := range lines[2:] {
		if e.Status != Skip {
			t.Errorf("%s: status %s, want skip", e.Name, e.Status)
			continue
		}
		if e.Detail != "needs base image" {
			t.Errorf("%s: detail %q, want %q", e.Name, e.Detail, "needs base image")
		}
		_ = i
	}
	if !strings.Contains(rep.String(), "✗ base image: elixir-release is not in the catalog") {
		t.Errorf("report does not name the missing image:\n%s", rep)
	}
	if !strings.HasPrefix(rep.Next(), "set base_image to an image in the catalog") {
		t.Errorf("Next %q does not point at the remedy", rep.Next())
	}
}

// TestRunLaterFailKeepsNextFirst pins that the Next: line is always the
// FIRST failing check's remedy, never a later one.
func TestRunLaterFailKeepsNextFirst(t *testing.T) {
	f := passingFake()
	f.Images = []string{"go-base"}       // base image fails first
	f.PushErr = errors.New("403 denied") // key would fail too, but is skipped
	rep := runAll(t, f)
	if !strings.Contains(rep.Next(), "set base_image to an image in the catalog") {
		t.Errorf("Next %q is not the first failing check's remedy", rep.Next())
	}
	if strings.Contains(rep.String(), "403 denied") {
		t.Error("a skipped check leaked its error into the report")
	}
}

func TestRunGatesFail(t *testing.T) {
	f := passingFake()
	f.GateErr = errors.New("exit status 1")
	rep := runAll(t, f)
	if !rep.OK() == false {
		t.Error("failing report is OK")
	}
	if !strings.Contains(rep.String(), "✗ gates: mix format --check-formatted: exit status 1") {
		t.Errorf("report does not name the failing gate:\n%s", rep)
	}
	if !strings.HasPrefix(rep.Next(), "make mix format --check-formatted pass on the default branch") {
		t.Errorf("Next %q", rep.Next())
	}
}

func TestRunBudgetUnset(t *testing.T) {
	f := passingFake()
	f.BudgetResp = Budget{}
	rep := runAll(t, f)
	if !strings.Contains(rep.String(), "✗ budget: no budget is set") {
		t.Errorf("report does not say the budget is unset:\n%s", rep)
	}
	if !strings.HasPrefix(rep.Next(), "set a bee-hour budget for hrmny") {
		t.Errorf("Next %q", rep.Next())
	}
}

// TestRunOfflineEnvIsSkips runs the engine against the offline
// environment the CLI uses: the catalog lookup is real, and every
// host-only step skips with the reason that points at the check API.
func TestRunOfflineEnvIsSkips(t *testing.T) {
	env := NewOfflineEnv("", "", Instance{})
	p := validProject()
	rep := Run(context.Background(), &p, env)
	want := `✓ hive.yaml
✗ base image: list images: SPOOND_TOKEN is not set
  fix: point SPOOND_API at the lease API and set SPOOND_TOKEN to a valid consumer token, then run the check again.
- worker image: skipped (needs base image)
- deploy key: skipped (needs base image)
- trial lease: skipped (needs base image)
- gates: skipped (needs base image)
- budget: skipped (needs base image)
Next: point SPOOND_API at the lease API and set SPOOND_TOKEN to a valid consumer token, then run the check again.
`
	if got := rep.String(); got != want {
		t.Errorf("offline report:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunOfflineEnvHostStepsSkip(t *testing.T) {
	srv := newCatalogServer(t, "elixir-release")
	env := NewOfflineEnv(srv.URL, "token-a", testInstance())
	p := validProject()
	rep := Run(context.Background(), &p, env)
	want := `✓ hive.yaml
✓ base image
- worker image: skipped (runs on the host: POST /hive/check)
- deploy key: skipped (runs on the host: POST /hive/check)
- trial lease: skipped (runs on the host: POST /hive/check)
- gates: skipped (runs on the host: POST /hive/check)
- budget: skipped (runs on the host: POST /hive/check)
Next: enlist: POST /hive/projects
`
	if got := rep.String(); got != want {
		t.Errorf("offline report:\n%s\nwant:\n%s", got, want)
	}
	if !rep.OK() {
		t.Error("a report of passes and skips failed")
	}
}

func TestRunPassesDerivedAllowlistToLease(t *testing.T) {
	f := passingFake()
	runAll(t, f)
	reached := f.Reached()
	if len(reached) != 1 {
		t.Fatalf("trial lease ran %d times, want 1", len(reached))
	}
	want := []string{
		"code.example.com",
		"llm.example.com",
		"mail.example.com",
		"spoond.example.com:5000",
		"spoond.example.com:8890",
	}
	if !equalStrings(reached[0].allowlist, want) {
		t.Errorf("trial lease allowlist %v, want %v", reached[0].allowlist, want)
	}
	if !equalStrings(reached[0].needs, validProject().Needs) {
		t.Errorf("trial lease needs %v, want %v", reached[0].needs, validProject().Needs)
	}
}

func TestJSONShape(t *testing.T) {
	f := passingFake()
	f.Images = []string{"go-base"} // one failure, so remedy and next are set
	rep := runAll(t, f)
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Checks []struct {
			Name   string `json:"name"`
			Status Status `json:"status"`
			Detail string `json:"detail"`
			Remedy string `json:"remedy"`
		} `json:"checks"`
		Next string `json:"next"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Checks) != len(CheckNames) {
		t.Fatalf("%d checks, want %d", len(got.Checks), len(CheckNames))
	}
	for i, c := range got.Checks {
		if c.Name != CheckNames[i] {
			t.Errorf("check %d is %q, want %q", i, c.Name, CheckNames[i])
		}
	}
	if got.Checks[0].Status != Pass || got.Checks[1].Status != Fail {
		t.Errorf("statuses %+v, want pass then fail", got.Checks[:2])
	}
	if got.Checks[1].Remedy == "" {
		t.Error("failing check has no remedy in JSON")
	}
	for _, c := range got.Checks[2:] {
		if c.Status != Skip || c.Detail != "needs base image" {
			t.Errorf("%s: %+v, want skip with the dependency in its detail", c.Name, c)
		}
	}
	if got.OK {
		t.Error("ok is true with a failing check")
	}
	if got.Next != rep.Next() {
		t.Errorf("json next %q, want %q", got.Next, rep.Next())
	}
	if !strings.HasPrefix(got.Next, "set base_image to an image in the catalog") {
		t.Errorf("next %q", got.Next)
	}
	// Skips carry no remedy, passes carry no detail-only noise.
	if got.Checks[0].Remedy != "" {
		t.Errorf("passing check carries a remedy: %+v", got.Checks[0])
	}
}

// TestJSONAllPass pins the JSON of a report with nothing to fix: no
// remedies, and the enlistment route as next.
func TestJSONAllPass(t *testing.T) {
	rep := runAll(t, passingFake())
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Checks []map[string]any `json:"checks"`
		Next   string           `json:"next"`
		OK     bool             `json:"ok"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Error("ok is false")
	}
	if got.Next != NextEnlist {
		t.Errorf("next %q, want %q", got.Next, NextEnlist)
	}
	for i, c := range got.Checks {
		if _, has := c["remedy"]; has {
			t.Errorf("check %d carries a remedy: %v", i, c)
		}
	}
}

func TestReportLinesCopy(t *testing.T) {
	rep := runAll(t, passingFake())
	lines := rep.Lines()
	lines[0].Name = "mutated"
	if rep.Lines()[0].Name == "mutated" {
		t.Error("Lines() returned the report's own slice")
	}
}
