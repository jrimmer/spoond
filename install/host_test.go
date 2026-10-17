package install

import (
	"context"
	"errors"
	"testing"
)

// withHostSteps swaps the step table for a test and restores it after.
func withHostSteps(t *testing.T, steps map[string]hostStep) {
	t.Helper()
	saved := hostSteps
	hostSteps = steps
	t.Cleanup(func() { hostSteps = saved })
}

func TestHostPhaseRunsStepsInOrder(t *testing.T) {
	var ran []string
	steps := map[string]hostStep{}
	for _, name := range hostOrder {
		name := name
		steps[name] = func(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
			ran = append(ran, name)
			return nil
		}
	}
	withHostSteps(t, steps)
	env, _, _ := testEnv(t)
	if _, err := hostPhase(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(ran) != len(hostOrder) {
		t.Fatalf("ran %v, want %v", ran, hostOrder)
	}
	for i := range ran {
		if ran[i] != hostOrder[i] {
			t.Fatalf("ran %v, want %v", ran, hostOrder)
		}
	}
}

func TestHostPhaseRebootAfterAllSteps(t *testing.T) {
	var ran int
	steps := map[string]hostStep{}
	for _, name := range hostOrder {
		name := name
		steps[name] = func(ctx context.Context, env *Env, r *Reporter, st *hostState) error {
			ran++
			if name == "hugepages" {
				st.Hugepages = 5120
				st.RebootNeeded = true
			}
			return nil
		}
	}
	withHostSteps(t, steps)
	env, _, _ := testEnv(t)
	_, err := hostPhase(context.Background(), env)
	if !errors.Is(err, ErrRebootNeeded) {
		t.Fatalf("err %v, want ErrRebootNeeded", err)
	}
	if ran != len(hostOrder) {
		t.Fatalf("%d steps ran before the reboot error, want all %d", ran, len(hostOrder))
	}
}

func TestHostPhaseMissingStep(t *testing.T) {
	withHostSteps(t, map[string]hostStep{})
	env, _, _ := testEnv(t)
	if _, err := hostPhase(context.Background(), env); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err %v, want ErrNotImplemented", err)
	}
}
