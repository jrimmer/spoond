package fake

import (
	"testing"

	"github.com/jrimmer/spoond/v3/substrate"
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
	if len(f.CallLog()) != len(want) {
		t.Fatalf("Calls = %v, want %v", f.CallLog(), want)
	}
	for i, w := range want {
		if f.CallLog()[i] != w {
			t.Fatalf("Calls[%d] = %q, want %q", i, f.CallLog()[i], w)
		}
	}
}

// TestCreateFilesystemBoot: the fake keeps the request's FilesystemBoot
// for tests to read and reports it applied, as a capable orchestrator does.
func TestCreateFilesystemBoot(t *testing.T) {
	f := New()
	sb, err := f.Create(t.Context(), substrate.CreateRequest{SandboxID: "i0123456789abcdefghij", VCPU: 4, MemoryMB: 8192, FilesystemBoot: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sb.FilesystemBootApplied {
		t.Fatal("FilesystemBootApplied = false, want true")
	}
	if got := f.LastCreate(); !got.FilesystemBoot || got.VCPU != 4 || got.MemoryMB != 8192 {
		t.Fatalf("LastCreate = %+v, want FilesystemBoot at 4 vCPU / 8192 MB", got)
	}

	sb, err = f.Create(t.Context(), substrate.CreateRequest{SandboxID: "i1123456789abcdefghij"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.FilesystemBootApplied || f.LastCreate().FilesystemBoot {
		t.Fatal("snapshot resume reported FilesystemBoot")
	}
}
