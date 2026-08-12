// Pure formatting functions for the SSH gateway. No build constraint —
// these work on all platforms so tests can run without Linux.

package spoondgateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// prettySandboxTable renders the /api/sandboxes JSON as a columnar
// table (ticket #27). IDs show as a 12-char prefix (full id via --json).
func prettySandboxTable(b []byte) string {
	var resp struct {
		Sandboxes []map[string]any `json:"sandboxes"`
		Error     string           `json:"error"`
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
		return strings.TrimSpace(string(b)) // not our shape (or an error); pass through
	}
	if len(resp.Sandboxes) == 0 {
		return "no sandboxes"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-14s %-12s %-10s %-22s %-16s %s\n", "ID", "IMAGE", "STATE", "EXPIRES", "ADDRESS", "NAME")
	for _, s := range resp.Sandboxes {
		id, _ := s["id"].(string)
		img, _ := s["image"].(string)
		addr, _ := s["address"].(string)
		name, _ := s["name"].(string)
		if name == "" {
			name, _ = s["comment"].(string)
		}
		state := "running"
		if suspended, _ := s["suspended"].(bool); suspended {
			state = "suspended"
		}
		exp := ""
		if expUnix, ok := s["expires"].(float64); ok && expUnix > 0 {
			exp = time.Unix(int64(expUnix), 0).UTC().Format("2006-01-02 15:04 UTC")
		}
		if persistent, _ := s["persistent"].(bool); persistent {
			state += "*" // persistent lease: not TTL-swept
		}
		if len(id) > 12 {
			id = id[:12] + "…"
		}
		fmt.Fprintf(&sb, "%-14s %-12s %-10s %-22s %-16s %s\n", id, img, state, exp, addr, name)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// prettyStat renders the /stat JSON as a human-readable block
// (ticket #27).
func prettyStat(b []byte) string {
	var st struct {
		CPU struct {
			Load1 float64 `json:"load1"`
		} `json:"cpu"`
		Mem struct {
			UsedMiB  int64 `json:"used_mib"`
			TotalMiB int64 `json:"total_mib"`
		} `json:"mem"`
		Disk struct {
			UsedMiB  int64 `json:"used_mib"`
			TotalMiB int64 `json:"total_mib"`
		} `json:"disk"`
		Net struct {
			RXBytes int64 `json:"rx_bytes"`
			TXBytes int64 `json:"tx_bytes"`
		} `json:"net"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return strings.TrimSpace(string(b))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu : load1 %.2f\n", st.CPU.Load1)
	if st.Mem.TotalMiB > 0 {
		fmt.Fprintf(&sb, "mem : %d / %d MiB used\n", st.Mem.UsedMiB, st.Mem.TotalMiB)
	}
	if st.Disk.TotalMiB > 0 {
		fmt.Fprintf(&sb, "disk: %d / %d MiB used\n", st.Disk.UsedMiB, st.Disk.TotalMiB)
	}
	fmt.Fprintf(&sb, "net : rx %d bytes, tx %d bytes\n", st.Net.RXBytes, st.Net.TXBytes)
	return strings.TrimRight(sb.String(), "\n")
}
