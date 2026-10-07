// Pure formatting functions for the SSH gateway. No build constraint —
// these work on all platforms so tests can run without Linux.

package spoondgateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// prettySandboxTable renders the /api/leases JSON (the /api/sandboxes
// alias serves the same shape) as a columnar table (ticket #27). IDs
// show as a 12-char prefix (full id via --json).
func prettySandboxTable(b []byte) string {
	var resp struct {
		Sandboxes []map[string]any `json:"sandboxes"`
		Error     string           `json:"error"`
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
		return strings.TrimSpace(string(b)) // not our shape (or an error); pass through
	}
	if len(resp.Sandboxes) == 0 {
		return "no leases"
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

// snapshotSaveBody parses `snapshot save <lease> <name> [--key K] [--keep N]`
// arguments after the verb into the save request body. It returns the
// body and an error message ("" on success).
func snapshotSaveBody(args []string) (map[string]any, string) {
	// args is the snapshot subcommand's tail: save <lease> <name> …
	if len(args) < 3 {
		return nil, `usage: snapshot save <lease> <name> [--key K] [--keep N]`
	}
	body := map[string]any{"name": args[2]}
	rest := args[3:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--key":
			if i+1 >= len(rest) {
				return nil, `usage: --key needs a value`
			}
			body["idempotency_key"] = rest[i+1]
			i++
		case "--keep":
			if i+1 >= len(rest) {
				return nil, `usage: --keep needs a value`
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				return nil, fmt.Sprintf("invalid --keep %q: not a number", rest[i+1])
			}
			body["keep"] = n
			i++
		default:
			return nil, fmt.Sprintf("unknown option %q", rest[i])
		}
	}
	return body, ""
}

// namedSnapshotsPath builds the list path, adding ?prefix= when set.
func namedSnapshotsPath(prefix string) string {
	p := "/api/named-snapshots"
	if prefix != "" {
		p += "?prefix=" + url.QueryEscape(prefix)
	}
	return p
}

// namedSnapshotPath builds the show/delete path for a name[@version]
// reference. A slash in the name is left unescaped: the route's {name...}
// wildcard matches it.
func namedSnapshotPath(ref string, force bool) string {
	p := "/api/named-snapshots/" + ref
	if force {
		p += "?force=1"
	}
	return p
}

// parseCreateArgs parses `create [image] [--snapshot ref] [--ttl N]
// [--persistent]`. It returns the create request body. Either an image or
// a snapshot is required.
func parseCreateArgs(args []string) (map[string]any, string) {
	body := map[string]any{"persistent": true, "ttl": 3600}
	image := ""
	snapshot := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--snapshot":
			if i+1 >= len(args) {
				return nil, `usage: --snapshot needs a value`
			}
			snapshot = args[i+1]
			i++
		case "--ttl":
			if i+1 >= len(args) {
				return nil, `usage: --ttl needs a value`
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return nil, fmt.Sprintf("invalid --ttl %q", args[i+1])
			}
			body["ttl"] = n
			i++
		case "--persistent":
			body["persistent"] = true
		case "--ephemeral", "--non-persistent":
			body["persistent"] = false
		default:
			if strings.HasPrefix(args[i], "--") {
				return nil, fmt.Sprintf("unknown option %q", args[i])
			}
			if image != "" {
				return nil, fmt.Sprintf("unexpected argument %q", args[i])
			}
			image = args[i]
		}
	}
	if image == "" && snapshot == "" {
		return nil, `usage: create [image] [--snapshot <name[@v]>] [--ttl N] [--persistent]`
	}
	if image != "" {
		body["image"] = image
	}
	if snapshot != "" {
		body["snapshot"] = snapshot
	}
	return body, ""
}

// snapshotVersion is one version of a named snapshot (list/show shape).
type snapshotVersion struct {
	Version   int64  `json:"version"`
	BuildID   string `json:"build_id"`
	Image     string `json:"image"`
	MemoryMB  int64  `json:"memory_mb"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
	InUse     int64  `json:"in_use"`
	Stale     bool   `json:"stale"`
}

// snapshotName is one named snapshot with its versions.
type snapshotName struct {
	Name     string            `json:"name"`
	Latest   int64             `json:"latest"`
	Versions []snapshotVersion `json:"versions"`
}

// snapshotList is the shape of GET /api/named-snapshots.
type snapshotList struct {
	Snapshots []snapshotName `json:"snapshots"`
	Error     string         `json:"error"`
}

// prettySnapshots renders the named-snapshot list JSON as a columnar
// table; versions group under their name, newest first.
func prettySnapshots(b []byte) string {
	var resp snapshotList
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
		return strings.TrimSpace(string(b))
	}
	if len(resp.Snapshots) == 0 {
		return "no snapshots"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-22s %-7s %-8s %-14s %-12s %-10s %-21s %-6s %s\n",
		"NAME", "VERSION", "BUILD", "IMAGE", "MEMORY", "SIZE", "CREATED", "IN USE", "STALE")
	for _, n := range resp.Snapshots {
		if len(n.Versions) == 0 {
			fmt.Fprintf(&sb, "%-22s %-7s\n", n.Name, "-")
			continue
		}
		versions := append([]snapshotVersion(nil), n.Versions...)
		sort.Slice(versions, func(i, j int) bool { return versions[i].Version > versions[j].Version })
		for i, v := range versions {
			name := ""
			if i == 0 {
				name = n.Name
			}
			id := v.BuildID
			if len(id) > 12 {
				id = id[:12] + "…"
			}
			stale := "no"
			if v.Stale {
				stale = "yes"
			}
			created := v.CreatedAt
			if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
				created = t.UTC().Format("2006-01-02 15:04 UTC")
			}
			fmt.Fprintf(&sb, "%-22s %-7d %-8s %-14s %-12s %-10s %-21s %-6d %s\n",
				name, v.Version, id, v.Image, humanMiB(v.MemoryMB), humanBytes(v.SizeBytes), created, v.InUse, stale)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// humanMiB renders MiB compactly (e.g. 4 GiB).
func humanMiB(mib int64) string {
	if mib <= 0 {
		return "-"
	}
	if mib%1024 == 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// humanBytes renders a byte count in binary units.
func humanBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// prettySnapshotDetail renders one version object (show).
func prettySnapshotDetail(b []byte) string {
	var v struct {
		snapshotVersion
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Name == "" {
		return strings.TrimSpace(string(b))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "name     : %s@%d\n", v.Name, v.Version)
	fmt.Fprintf(&sb, "build_id : %s\n", v.BuildID)
	fmt.Fprintf(&sb, "image    : %s\n", v.Image)
	fmt.Fprintf(&sb, "memory   : %s\n", humanMiB(v.MemoryMB))
	fmt.Fprintf(&sb, "size     : %s\n", humanBytes(v.SizeBytes))
	fmt.Fprintf(&sb, "created  : %s\n", v.CreatedAt)
	fmt.Fprintf(&sb, "in use   : %d\n", v.InUse)
	fmt.Fprintf(&sb, "stale    : %v\n", v.Stale)
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
