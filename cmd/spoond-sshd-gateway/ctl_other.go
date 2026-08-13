//go:build !linux

// Non-Linux stub for runControlCommand so tests compile on macOS.
// The real implementation (in main.go, //go:build linux) handles all
// ctl verbs via backend API calls. This stub handles only the pure
// string-manipulation verbs that the unit tests exercise (whoami,
// ssh-key usage); other verbs return an error string.

package spoondgateway

import (
	"context"
	"fmt"
	"strings"
)

func runControlCommand(ctx context.Context, cmd string, gatewayKey interface{}, keyID, userID, userName string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return `{"error":"empty command"}`
	}
	jsonMode := false
	kept := fields[:0]
	for _, f := range fields {
		if f == "--json" || f == "-j" {
			jsonMode = true
			continue
		}
		kept = append(kept, f)
	}
	fields = kept

	switch fields[0] {
	case "whoami":
		if keyID == "" {
			if jsonMode {
				return `{"user":"ctl","key":"unknown"}`
			}
			return "user: ctl (key: unknown)"
		}
		if userName != "" {
			if jsonMode {
				return fmt.Sprintf(`{"user":%q,"key":%q,"user_id":%q}`, userName, keyID, userID)
			}
			return fmt.Sprintf("user: %s (key: %s)", userName, keyID)
		}
		if jsonMode {
			return fmt.Sprintf(`{"user":"ctl","key":%q}`, keyID)
		}
		return fmt.Sprintf("user: ctl (key: %s)", keyID)
	case "ssh-key", "keys":
		return "usage: ssh-key ls|add <pubkey> <name>|rm <id>"
	case "help", "--help", "-h":
		return "commands: new, ls, rm, stat, whoami, help (stub — full impl on Linux)"
	default:
		return fmt.Sprintf(`{"error":"stub: %s not available on non-Linux"}`, fields[0])
	}
}
