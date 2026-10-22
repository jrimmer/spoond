// The doctor phase: the last row of the install phase table
// (docs/plans/2026-10-16-4.0-install.md, S6b). It runs `spoond doctor`
// over the installed result — orchestrator, registry, LLM upstream,
// listeners, the image catalog — and the install fails when doctor fails, so a
// half-installed host never reports success.
//
// The command runs through systemd-run with the backend's env files as
// `-p EnvironmentFile=` properties: systemd reads the files and puts
// their values in doctor's environment, so no secret ever sits on a
// command line, and this phase reads backend.env nowhere.
//
// In a container or a dry run the phase runs nothing and reports one
// skipped row: in the container the spoond service runs in compose, so
// the check belongs to `docker compose exec spoond spoond doctor`, and
// a dry run installs nothing, so there is no result to check yet.
package install

import (
	"context"
	"errors"
	"fmt"
)

const (
	// doctorRow is the phase's single report row.
	doctorRow = "doctor"
	// doctorBackendEnv is the env file systemd loads before running
	// doctor (the spoond phase wrote it; doctor reads its values from
	// the process environment, the phase never does).
	doctorBackendEnv = "/etc/spoond/backend.env"
	// doctorLLMEnv is the optional LLM-key env file: the leading
	// minus on its EnvironmentFile means systemd carries on when it
	// does not exist.
	doctorLLMEnv = "/etc/spoond/llm.env"
)

// doctorPhase implements the doctor row of the phase table.
func init() { Phases[PhaseDoctor] = doctorPhase }

func doctorPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseDoctor, Out: env.Out}

	// In the Docker image the spoond service runs in compose, so
	// this phase has nothing to check: the check belongs inside the
	// container, against the service's own environment.
	if env.Opt.Container {
		r.Add(Skipped, doctorRow, "--container: run `docker compose exec spoond spoond doctor`")
		return r.Items(), nil
	}
	// A dry run runs nothing, so there is no result for doctor to
	// run over: say so and stop.
	if env.DryRun {
		r.Add(Skipped, doctorRow, "dry-run: doctor not run")
		return r.Items(), nil
	}

	// The check itself, directly through env.Run (a query needs the
	// live host anyway): systemd reads the env files — llm.env
	// optionally — and no secret lands on a command line.
	out, err := env.Run.Run(ctx,
		"systemd-run",
		"--wait", "--pipe", "--quiet", "--collect",
		// The backend unit's order, so the same value wins.
		"-p", "EnvironmentFile=-"+doctorLLMEnv,
		"-p", "EnvironmentFile="+doctorBackendEnv,
		"/opt/spoond/spoond",
		"doctor",
	)

	// Doctor's own stdout is the PASS/FAIL/WARN table: write it as
	// it is, before the verdict row, so a failure shows the checks
	// that failed and the row below says what to do next.
	if len(out) > 0 && env.Out != nil {
		if _, werr := fmt.Fprint(env.Out, string(out)); werr != nil {
			return r.Items(), werr
		}
	}
	if err != nil {
		note := "spoond doctor reports failures (table above); fix them and run `spoond install doctor` again"
		r.Add(Failed, doctorRow, note)
		return r.Items(), errors.New(note)
	}
	r.Add(Kept, doctorRow, "nothing failing")
	return r.Items(), nil
}
