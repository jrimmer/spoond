// Tests for the doctor phase (doctor.go): a fake runner records the
// systemd-run call and answers from a table. Every test pins one rule:
// the exact command line (env files as systemd properties, no secret on
// any argv); the container and dry-run rows run nothing; a failing
// doctor's table still reaches env.Out and the phase fails with the
// rerun fix; a passing doctor is one kept row.
package install

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// doctorTestEnv returns an Env with a buffering Out and a recording
// runner for the doctor phase (a temp Root is not needed: the phase
// reads no files and resolves no paths).
func doctorTestEnv() (*Env, *strings.Builder, *recordRunner) {
	out := &strings.Builder{}
	runner := &recordRunner{}
	env := &Env{Run: runner, Out: out}
	return env, out, runner
}

// runDoctorPhase runs the doctor phase and returns its rows, error and
// buffers.
func runDoctorPhase(t *testing.T, env *Env) ([]Item, error) {
	t.Helper()
	return RunPhase(context.Background(), env, PhaseDoctor, Phases[PhaseDoctor])
}

// doctorRunnerReply is the fake's reply table: the doctor command's
// stdout (the PASS/FAIL/WARN table) and error.
func doctorRunnerReply(out []byte, err error) map[string]fakeReply {
	return map[string]fakeReply{
		"systemd-run --wait --pipe --quiet --collect " +
			"-p EnvironmentFile=-/etc/spoond/llm.env " +
			"-p EnvironmentFile=/etc/spoond/backend.env " +
			"/opt/spoond/spoond doctor": {out: out, err: err},
	}
}

// errFakeDoctor is the fake doctor's failure: the runner's own error
// text, which the phase replaces with the rerun fix on the row.
var errFakeDoctor = errors.New("/opt/spoond/spoond doctor: exit status 1")

// doctorTable is `spoond doctor` stdout in the shape Main prints: the
// rows plus the trailing verdict line.
const doctorTable = "PASS  config: CONSUMER_TOKENS          set\n" +
	"WARN  storage: headroom                  97 GiB free\n" +
	"\nall checks passed\n"

// TestDoctorPhaseCommandPinned pins the exact command line and the
// kept row: the systemd-run wrapper with the backend env file, the
// optional llm.env (the leading minus) and the installed binary's
// doctor, and nothing else. No secret value is on it.
func TestDoctorPhaseCommandPinned(t *testing.T) {
	env, out, runner := doctorTestEnv()
	runner.reply = doctorRunnerReply([]byte(doctorTable), nil)

	items, err := runDoctorPhase(t, env)
	if err != nil {
		t.Fatalf("doctor phase: %v", err)
	}
	want := "systemd-run --wait --pipe --quiet --collect " +
		"-p EnvironmentFile=-/etc/spoond/llm.env " +
		"-p EnvironmentFile=/etc/spoond/backend.env " +
		"/opt/spoond/spoond doctor"

	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("ran %v, want exactly\n  %s", runner.calls, want)
	}
	if len(items) != 1 || items[0].Result != Kept || items[0].Name != doctorRow {
		t.Fatalf("rows = %+v, want one kept %q row", items, doctorRow)
	}
	if !strings.Contains(out.String(), "nothing failing") {
		t.Errorf("report does not say what was kept:\n%s", out.String())
	}
}

// TestDoctorPhaseTableToOut: doctor's stdout reaches env.Out as it is,
// above the verdict row, whether it passes or fails.
func TestDoctorPhaseTableToOut(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		env, out, runner := doctorTestEnv()
		runner.reply = doctorRunnerReply([]byte(doctorTable), nil)
		if _, err := runDoctorPhase(t, env); err != nil {
			t.Fatalf("doctor phase: %v", err)
		}
		if !strings.Contains(out.String(), doctorTable) {
			t.Errorf("doctor's table did not reach Out:\n%s", out.String())
		}
	})

	t.Run("fail", func(t *testing.T) {
		env, out, runner := doctorTestEnv()
		runner.reply = doctorRunnerReply([]byte("FAIL  orchestrator: health        connection refused\n"), errFakeDoctor)
		items, err := runDoctorPhase(t, env)
		if err == nil {
			t.Fatal("phase succeeded, want the failure")
		}
		wantErr := "doctor: spoond doctor reports failures (table above); fix them and run `spoond install doctor` again"
		if err.Error() != wantErr {
			t.Errorf("err = %q, want %q", err, wantErr)
		}
		if !strings.Contains(out.String(), "FAIL  orchestrator: health") {
			t.Errorf("the failing doctor's table did not reach Out:\n%s", out.String())
		}
		if got := items; len(got) != 1 || got[0].Result != Failed || got[0].Name != doctorRow ||
			got[0].Note != "spoond doctor reports failures (table above); fix them and run `spoond install doctor` again" {
			t.Errorf("rows = %+v, want one failed %q row with the rerun fix", got, doctorRow)
		}
	})
}

// TestDoctorPhaseContainerSkipped pins the container rule: the row
// names the compose exec instead, and no command runs.
func TestDoctorPhaseContainerSkipped(t *testing.T) {
	env, out, runner := doctorTestEnv()
	env.Opt.Container = true

	items, err := runDoctorPhase(t, env)
	if err != nil {
		t.Fatalf("doctor phase: %v", err)
	}
	if len(items) != 1 || items[0].Result != Skipped || items[0].Name != doctorRow {
		t.Fatalf("rows = %+v, want one skipped %q row", items, doctorRow)
	}
	want := "--container: run `docker compose exec spoond spoond doctor`"
	if items[0].Note != want {
		t.Errorf("note = %q, want %q", items[0].Note, want)
	}
	if !strings.Contains(out.String(), want) {
		t.Errorf("report, want the container note:\n%s", out.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("the container run ran commands: %v", runner.calls)
	}
}

// TestDoctorPhaseDryRunSkipped pins the dry run: one skipped row,
// nothing runs.
func TestDoctorPhaseDryRunSkipped(t *testing.T) {
	env, out, runner := doctorTestEnv()
	env.DryRun = true

	items, err := runDoctorPhase(t, env)
	if err != nil {
		t.Fatalf("doctor phase: %v", err)
	}
	if len(items) != 1 || items[0].Result != Skipped || items[0].Name != doctorRow {
		t.Fatalf("rows = %+v, want one skipped %q row", items, doctorRow)
	}
	want := "dry-run: doctor not run"
	if items[0].Note != want {
		t.Errorf("note = %q, want %q", items[0].Note, want)
	}
	if !strings.Contains(out.String(), want) {
		t.Errorf("report, want the dry-run note:\n%s", out.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("the dry run ran commands: %v", runner.calls)
	}
}
