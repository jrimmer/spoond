package fake

import (
	"testing"

	"github.com/jrimmer/spoond/substrate"
)

// Compile checks: the fake implements the substrate interfaces.
var (
	_ substrate.Substrate = (*Fake)(nil)
	_ substrate.Process   = (*FakeProcess)(nil)
)

func TestCallLog(t *testing.T) {
	f := New()

	id := "i0123456789abcdefghij"
	if _, err := f.Create(t.Context(), substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Health(t.Context(), id); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if _, err := f.NodeInfo(t.Context()); err != nil {
		t.Fatalf("NodeInfo: %v", err)
	}
	if _, _, err := f.Pause(t.Context(), id, "template0000000000000"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	want := []string{
		"Create " + id,
		"Health " + id,
		"NodeInfo",
		"Pause " + id,
	}
	if len(f.Calls) != len(want) {
		t.Fatalf("Calls = %v, want %v", f.Calls, want)
	}
	for i, w := range want {
		if f.Calls[i] != w {
			t.Fatalf("Calls[%d] = %q, want %q", i, f.Calls[i], w)
		}
	}
}
