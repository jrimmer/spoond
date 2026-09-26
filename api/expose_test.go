package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateExposePorts(t *testing.T) {
	tests := []struct {
		name    string
		in      []int
		want    []int
		wantErr string
	}{
		{"empty", nil, []int{}, ""},
		{"sorted", []int{9042, 80}, []int{80, 9042}, ""},
		{"zero", []int{0}, nil, "out of range"},
		{"too high", []int{65536}, nil, "out of range"},
		{"guest agent", []int{8888}, nil, "cannot be exposed"},
		{"shelley", []int{9000}, nil, "cannot be exposed"},
		{"duplicate", []int{9042, 9042}, nil, "listed twice"},
		{"too many", []int{1, 2, 3, 4, 5, 6, 7, 8, 9}, nil, "at most"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateExposePorts(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExposeCommands(t *testing.T) {
	// No ports still flushes: a reused netns must lose a previous tenant's DNAT.
	if got := exposeCommands("10.42.0.2", nil); len(got) != 1 || strings.Join(got[0], " ") != "-t nat -F PREROUTING" {
		t.Fatalf("empty exposure must be exactly the PREROUTING flush, got %v", got)
	}

	cmds := exposeCommands("10.42.0.2", []int{9042})
	if len(cmds) != 4 {
		t.Fatalf("want flush + 3 rules per port, got %d: %v", len(cmds), cmds)
	}
	joined := make([]string, len(cmds))
	for i, c := range cmds {
		joined[i] = strings.Join(c, " ")
	}
	if joined[0] != "-t nat -F PREROUTING" {
		t.Fatalf("first command must flush PREROUTING, got %q", joined[0])
	}
	want := []string{
		"-t nat -A PREROUTING -i veth0 -p tcp --dport 9042 -m comment --comment forkd-expose -j DNAT --to-destination 10.42.0.2:9042",
		// Inserted at the top so they precede the policy's final DROP.
		"-I FORWARD 1 -i veth0 -o forkd-tap0 -p tcp -d 10.42.0.2 --dport 9042 -m comment --comment forkd-expose -j ACCEPT",
		// Replies only: the guest cannot originate traffic through this rule.
		"-I FORWARD 1 -i forkd-tap0 -o veth0 -p tcp -s 10.42.0.2 --sport 9042 -m state --state ESTABLISHED -m comment --comment forkd-expose -j ACCEPT",
	}
	for i, w := range want {
		if joined[i+1] != w {
			t.Fatalf("rule %d:\n got %q\nwant %q", i+1, joined[i+1], w)
		}
	}
}

func TestParseIPv4Addr(t *testing.T) {
	out := "3: veth0    inet 10.43.0.10/16 brd 10.43.255.255 scope global veth0       valid_lft forever preferred_lft forever\n"
	if got := parseIPv4Addr(out); got != "10.43.0.10" {
		t.Fatalf("got %q", got)
	}
	if got := parseIPv4Addr(""); got != "" {
		t.Fatalf("empty output must parse to empty, got %q", got)
	}
}

// fakeExposer is a fakeNetpol that can also publish ports.
type fakeExposer struct {
	fakeNetpol
	exposed []string
	ip      string
}

func (f *fakeExposer) Expose(_ context.Context, netns, guestHost string, ports []int) (string, error) {
	f.exposed = append(f.exposed, fmt.Sprintf("%s|%s|%v", netns, guestHost, ports))
	return f.ip, nil
}

// TestApplyNetpolExposesPorts: exposure rides every policy application —
// grant and resume — and runs (as a flush) even when nothing is exposed.
func TestApplyNetpolExposesPorts(t *testing.T) {
	ff := newFakeForkd()
	ff.netns = "forkd-child-9"
	svc := NewService(ff, map[string]string{"t": "c"}, 0, time.Minute, 10*time.Minute, "py-base")
	fe := &fakeExposer{ip: "10.43.0.10"}
	svc.SetNetpol(fe, []string{"10.1.0.2"})

	l, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, true, string(PolicyNone), nil, 9042)
	if err != nil {
		t.Fatal(err)
	}
	if len(fe.exposed) != 1 || !strings.HasSuffix(fe.exposed[0], "|10.42.0.2|[9042]") {
		t.Fatalf("grant must expose 9042 to the guest, got %v", fe.exposed)
	}
	if got := exposedMap(l); got["9042"] != "10.43.0.10:9042" {
		t.Fatalf("exposed map: %v", got)
	}

	fe.exposed = nil
	if _, err := svc.resume(context.Background(), "c", l.ID); err != nil {
		t.Fatal(err)
	}
	if len(fe.exposed) != 1 {
		t.Fatalf("resume must re-expose (new netns), got %v", fe.exposed)
	}

	fe.exposed = nil
	plain, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fe.exposed) != 1 || !strings.HasSuffix(fe.exposed[0], "|[]") {
		t.Fatalf("a lease with no ports must still flush stale DNAT, got %v", fe.exposed)
	}
	if len(exposedMap(plain)) != 0 {
		t.Fatalf("no ports means no exposed addresses, got %v", exposedMap(plain))
	}
}

// TestApplyNetpolRefusesExposureWithoutCapability: an applier that cannot
// publish must fail a lease that asked for ports rather than hand it out
// silently unreachable.
func TestApplyNetpolRefusesExposureWithoutCapability(t *testing.T) {
	ff := newFakeForkd()
	ff.netns = "forkd-child-9"
	svc := NewService(ff, map[string]string{"t": "c"}, 0, time.Minute, 10*time.Minute, "py-base")
	svc.SetNetpol(&fakeNetpol{}, nil)
	if _, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, false, "", nil, 9042); err == nil {
		t.Fatal("expected grant to fail when the applier cannot expose ports")
	}
}

func TestCreateExposePortsAPI(t *testing.T) {
	build := func(t *testing.T, withExposer bool) *httptest.Server {
		t.Helper()
		ff := newFakeForkd()
		ff.netns = "forkd-child-3"
		svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute)
		if withExposer {
			svc.SetNetpol(&fakeExposer{ip: "10.43.0.4"}, []string{"10.1.0.2"})
		}
		ts := httptest.NewServer(NewServer(svc, NewImageRegistry(ff, "py-base")).Handler())
		t.Cleanup(ts.Close)
		return ts
	}

	t.Run("reserved port is refused", func(t *testing.T) {
		ts := build(t, true)
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{8888}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
	})

	t.Run("no enforcement means no exposure", func(t *testing.T) {
		ts := build(t, false)
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{9042}})
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
	})

	t.Run("published address is returned", func(t *testing.T) {
		ts := build(t, true)
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "network_policy": "none", "expose_ports": []int{9042}})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
		exposed, _ := body["exposed"].(map[string]any)
		if exposed["9042"] != "10.43.0.4:9042" {
			t.Fatalf("exposed: %v", body["exposed"])
		}
	})
}
