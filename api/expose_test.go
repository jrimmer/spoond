package api

import (
	"fmt"
	"net/http"
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
		{"envd", []int{49983}, nil, "cannot be exposed"},
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

// Exposure is always supported on the E2B substrate (U09): a create with
// expose_ports succeeds; the reserved envd port fails validation with 400.
func TestCreateExposePorts(t *testing.T) {
	ts, _ := newTestServer(t)

	t.Run("reserved port is refused", func(t *testing.T) {
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{49983}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
	})

	t.Run("exposure is supported", func(t *testing.T) {
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "expose_ports": []int{9042, 80}})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d, body %v", resp.StatusCode, body)
		}
		if _, ok := body["exposed"].(map[string]any); !ok {
			t.Fatalf("create response missing exposed map: %v", body)
		}
	})
}
