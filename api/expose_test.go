package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// Until U09 (envd-based exposure) no backend can publish guest ports, so
// create answers 501 for expose_ports; reserved ports still fail
// validation with 400.
func TestCreateExposePortsInterim(t *testing.T) {
	build := func(t *testing.T) *httptest.Server {
		t.Helper()
		ts, _ := newTestServer(t)
		return ts
	}

	t.Run("reserved port is refused", func(t *testing.T) {
		ts := build(t)
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{8888}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
	})

	t.Run("no enforcement means no exposure", func(t *testing.T) {
		ts := build(t)
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{9042}})
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
	})
}
