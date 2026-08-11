package api

import (
	"testing"
	"time"
)

// TestLookupUserScoped verifies the forward-auth proxy lookup helper
// (U7/T7): it resolves only the owner's leases by id or friendly name.
func TestLookupUserScoped(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 0, 60*time.Second, 10*time.Minute)

	// two users each create a named lease
	lA, err := svc.grant(t.Context(), "u-a", "py-base", 0, time.Minute, true, "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	svc.store.mu.Lock()
	lA.Name = "web"
	svc.store.mu.Unlock()

	lB, err := svc.grant(t.Context(), "u-b", "py-base", 0, time.Minute, true, "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}
	svc.store.mu.Lock()
	lB.Name = "otherweb"
	svc.store.mu.Unlock()

	// u-a resolves its own id + name
	if got := svc.lookupUserScoped("u-a", lA.ID); got == nil || got.ID != lA.ID {
		t.Fatalf("owner id lookup failed")
	}
	if got := svc.lookupUserScoped("u-a", "web"); got == nil || got.ID != lA.ID {
		t.Fatalf("owner name lookup failed")
	}
	// u-a cannot resolve u-b's lease by id or name
	if got := svc.lookupUserScoped("u-a", lB.ID); got != nil {
		t.Fatalf("cross-owner id leak: %v", got)
	}
	if got := svc.lookupUserScoped("u-a", "otherweb"); got != nil {
		t.Fatalf("cross-owner name leak: %v", got)
	}
	// u-b resolves its own
	if got := svc.lookupUserScoped("u-b", "otherweb"); got == nil || got.ID != lB.ID {
		t.Fatalf("owner b name lookup failed")
	}
	// unknown owner / label
	if got := svc.lookupUserScoped("u-nobody", "web"); got != nil {
		t.Fatalf("unknown owner leak: %v", got)
	}
}
