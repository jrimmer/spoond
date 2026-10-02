// fake.go is the Env the tests run the checks against (and the one the
// server's tests will reuse in step 3): every side effect is recorded
// and answered from a script, so the engine's order, its dependency
// skips and its rendering can be asserted without touching a host, a
// repo, a lease or a budget.
package hive

import (
	"context"
	"fmt"
	"sync"
)

// FakeEnv is a scripted Env. Each step either succeeds, returns the
// error set for it, or — for Build, Key, Lease, Gates and Budget —
// skips with the reason a host-only Env would give.
type FakeEnv struct {
	// Inst is what Facts returns.
	Inst Instance
	// Images is the catalog ListImages returns.
	Images []string
	// ListErr, when set, is returned by ListImages.
	ListErr error
	// BuildErr, when set, is returned by BuildImage.
	BuildErr error
	// PushErr, when set, is returned by PushScratch.
	PushErr error
	// ReachErr, when set, is returned by Reachable.
	ReachErr error
	// GateErr, when set, is returned by RunGate.
	GateErr error
	// BudgetResp is what Budget returns.
	BudgetResp Budget
	// BudgetErr, when set, is returned by Budget.
	BudgetErr error
	// HostOnly makes the host steps return a SkipError, like OfflineEnv.
	HostOnly bool

	mu     sync.Mutex
	called []string
	built  []string
	reach  []struct {
		allowlist []string
		needs     []string
	}
	gates []string
}

// record notes a call, so tests can assert the order the engine ran.
func (f *FakeEnv) record(step string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, step)
}

// hostStep returns the host-only skip when the fake is scripted that
// way, or nil.
func (f *FakeEnv) hostStep(step string) error {
	if !f.HostOnly {
		return nil
	}
	return Skipf("%s", step)
}

// Calls returns the steps the engine ran, in order.
func (f *FakeEnv) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.called...)
}

// Built returns the worker images the engine asked to build.
func (f *FakeEnv) Built() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.built...)
}

// Reached returns one entry per trial lease, with the allowlist and
// needs the engine passed.
func (f *FakeEnv) Reached() []struct {
	allowlist []string
	needs     []string
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct {
		allowlist []string
		needs     []string
	}, len(f.reach))
	copy(out, f.reach)
	return out
}

// GateCommands returns the gates the engine ran, in order.
func (f *FakeEnv) GateCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gates...)
}

// Facts returns the scripted instance facts.
func (f *FakeEnv) Facts(ctx context.Context) (Instance, error) {
	return f.Inst, nil
}

// ListImages returns the scripted catalog.
func (f *FakeEnv) ListImages(ctx context.Context) ([]string, error) {
	f.record("images")
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return append([]string(nil), f.Images...), nil
}

// BuildImage records the image and returns the scripted outcome.
func (f *FakeEnv) BuildImage(ctx context.Context, image string) error {
	f.record("build")
	if err := f.hostStep("runs on the host: POST /hive/check"); err != nil {
		return err
	}
	f.mu.Lock()
	f.built = append(f.built, image)
	f.mu.Unlock()
	return f.BuildErr
}

// PushScratch records the repo and returns the scripted outcome.
func (f *FakeEnv) PushScratch(ctx context.Context, repo string) error {
	f.record("key")
	if err := f.hostStep("runs on the host: POST /hive/check"); err != nil {
		return err
	}
	return f.PushErr
}

// Reachable records the allowlist and needs and returns the scripted
// outcome.
func (f *FakeEnv) Reachable(ctx context.Context, allowlist, needs []string) error {
	f.record("lease")
	if err := f.hostStep("runs on the host: POST /hive/check"); err != nil {
		return err
	}
	f.mu.Lock()
	f.reach = append(f.reach, struct {
		allowlist []string
		needs     []string
	}{append([]string(nil), allowlist...), append([]string(nil), needs...)})
	f.mu.Unlock()
	return f.ReachErr
}

// RunGate records the gate and returns the scripted outcome.
func (f *FakeEnv) RunGate(ctx context.Context, repo, gate string) error {
	f.record("gate")
	if err := f.hostStep("runs on the host: POST /hive/check"); err != nil {
		return err
	}
	f.mu.Lock()
	f.gates = append(f.gates, gate)
	f.mu.Unlock()
	return f.GateErr
}

// Budget returns the scripted budget.
func (f *FakeEnv) Budget(ctx context.Context, project string) (Budget, error) {
	f.record("budget")
	if err := f.hostStep("runs on the host: POST /hive/check"); err != nil {
		return Budget{}, err
	}
	if f.BudgetErr != nil {
		return Budget{}, f.BudgetErr
	}
	return f.BudgetResp, nil
}

// String renders the fake's call log, for test failure messages.
func (f *FakeEnv) String() string {
	return fmt.Sprintf("FakeEnv calls=%v", f.Calls())
}
