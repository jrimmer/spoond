//go:build !nonotify

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jrimmer/spoond/v2/notify"
)

func init() {
	register(command{
		name: "notify",
		desc: "webhook notification tools (NOTIFY_WEBHOOKS)",
		run:  notifyMain,
	})
}

// notifyMain dispatches `spoond notify <sub>`: today only `test`, which
// posts one info message to every configured webhook and reports
// per-webhook outcomes. URLs and headers may carry secrets: only
// redacted scheme://host forms and webhook indexes are ever printed.
func notifyMain(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, "usage: spoond notify test [flags]\n\n  test   send one info message to every NOTIFY_WEBHOOKS webhook\n")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "test":
		return notifyTest(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "spoond notify: unknown subcommand %q (want test)\n", args[0])
		return 2
	}
}

func notifyTest(args []string) int {
	fs := flag.NewFlagSet("notify test", flag.ExitOnError)
	timeoutSecs := fs.Int("timeout", 10, "per-webhook request timeout, seconds")
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	_ = fs.Parse(args)

	hooks, err := notify.ParseWebhooks(os.Getenv("NOTIFY_WEBHOOKS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "spoond notify test:", err)
		return 2
	}
	if len(hooks) == 0 {
		fmt.Fprintln(os.Stderr, "spoond notify test: NOTIFY_WEBHOOKS is unset or empty — nothing to test")
		return 1
	}
	results := notify.SendTest(context.Background(), hooks, time.Duration(*timeoutSecs)*time.Second, log.Default())
	failed := 0
	if *jsonOut {
		type out struct {
			Webhook int    `json:"webhook"`
			URL     string `json:"url"`
			OK      bool   `json:"ok"`
			Error   string `json:"error,omitempty"`
		}
		o := make([]out, len(results))
		for i, r := range results {
			o[i] = out{Webhook: r.Webhook, URL: r.Redacted, OK: r.Err == nil}
			if r.Err != nil {
				o[i].Error = r.Err.Error()
			}
		}
		b, _ := json.MarshalIndent(o, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, r := range results {
			status, detail := "PASS", ""
			if r.Err != nil {
				status, detail = "FAIL", r.Err.Error()
				failed++
			}
			s := fmt.Sprintf("%-5s webhook %d (%s)", status, r.Webhook, r.Redacted)
			if detail != "" {
				s += ": " + detail
			}
			fmt.Println(s)
		}
	}
	if failed > 0 {
		fmt.Printf("\n%d of %d webhook(s) failed\n", failed, len(results))
		return 1
	}
	fmt.Printf("\nall %d webhook(s) delivered\n", len(results)-failed)
	return 0
}
