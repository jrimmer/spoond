package api

// The ordered memory-pressure reclaim policy (#145 D1): PRESSURE_ORDER
// is validated at startup, an unknown step is a configuration error, and
// the default order is the three documented steps.

import (
	"strings"
	"testing"
)

// TestParsePressureOrderValid: the default and any subset/order of the
// known steps parse.
func TestParsePressureOrderValid(t *testing.T) {
	steps, err := ParsePressureOrder("")
	if err != nil {
		t.Fatalf("empty = %v, want the default", err)
	}
	if len(steps) != 3 || steps[0] != PressureStepBurstUnheld || steps[2] != PressureStepGuaranteedUnheldIdle {
		t.Fatalf("default order = %v", steps)
	}

	steps, err = ParsePressureOrder(" guaranteed-unheld-idle , burst-held ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != 2 || steps[0] != PressureStepGuaranteedUnheldIdle || steps[1] != PressureStepBurstHeld {
		t.Fatalf("parsed order = %v", steps)
	}

	if steps, err := ParsePressureOrder("guaranteed-unheld-idle"); err != nil || len(steps) != 1 {
		t.Fatalf("single step = %v, %v", steps, err)
	}
}

// TestParsePressureOrderInvalidStep: an unknown, empty or repeated step
// is an error, so the backend can treat it as a fatal configuration
// error at startup.
func TestParsePressureOrderInvalidStep(t *testing.T) {
	cases := map[string]string{
		"burst-unheld,typo-step":   "unknown step",
		"burst-unheld,,burst-held": "empty step",
		"burst-held,burst-held":    "duplicate step",
		",":                        "empty step",
	}
	for in, want := range cases {
		_, err := ParsePressureOrder(in)
		if err == nil {
			t.Fatalf("%q parsed, want an error", in)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q error = %v, want it to contain %q", in, err, want)
		}
	}
}

// TestPressureOrderUnknownStepErrorMessage names the bad step.
func TestPressureOrderUnknownStepErrorMessage(t *testing.T) {
	_, err := ParsePressureOrder("burst-unheld,typo-step")
	if err == nil || !strings.Contains(err.Error(), "typo-step") {
		t.Fatalf("error = %v, want it to name the unknown step", err)
	}
}
