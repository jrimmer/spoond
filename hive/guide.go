// guide.go renders the enlistment guide (C11) from live values: the
// instance's facts, its image catalog, the schema's field table, the
// needs: table, the /hive/ route table and the checks. The server owns
// the route table and calls Guide; nothing here is prose about a value
// the caller did not pass in.
package hive

import (
	"fmt"
	"sort"
	"strings"
)

// Route is one /hive/ route as the guide describes it. The caller's
// route table — the one that also registers the routes on its mux — is
// rendered verbatim, so a route cannot exist without being taught and
// cannot be taught without existing.
type Route struct {
	// Method is the HTTP method.
	Method string `json:"method"`
	// Path is the route's path.
	Path string `json:"path"`
	// Auth is who may call it.
	Auth string `json:"auth"`
	// What is one line saying what the route does.
	What string `json:"what"`
}

// RouteAuthNone and RouteAuthToken are the guide's two auth levels.
const (
	// RouteAuthNone is the guide's route: no token, LAN only.
	RouteAuthNone = "none (LAN)"
	// RouteAuthToken is a consumer token, as the /api routes.
	RouteAuthToken = "consumer token"
)

// GuideInput is everything the guide renders: the instance facts (with
// the image catalog filled in), the lease API base URL as a caller
// reaches it, the guest-service URL, and the route table.
type GuideInput struct {
	// Inst carries the instance facts; Inst.Images is the catalog.
	Inst Instance `json:"instance"`
	// BaseURL is the lease API base URL (scheme://host:port).
	BaseURL string `json:"base_url"`
	// GuestServiceURL is the guest-service URL sandboxes reach.
	GuestServiceURL string `json:"guest_service_url"`
	// Routes are the /hive/ routes, in guide order.
	Routes []Route `json:"routes"`
}

// guideIntro is what the hive is: three sentences, the only prose in
// the guide that does not render a value the caller passed in.
const guideIntro = `The hive runs a project's agent workers (bees) as leases on this spoond
instance: it holds the task graph, spawns bees for ready work, hands them
tasks over Agent Mail and stops them when they idle out. A project joins by
describing itself in .spoond/hive.yaml in its own repository, and everything
else — the worker image, the network allowlist, the credentials — is derived
from that file. Point an agent at this guide and it has everything it needs
to take the next step.`

// Guide renders the guide as Markdown, from the values in in. It ends
// with exactly one Next: line.
func Guide(in GuideInput) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Enlisting a project in the hive")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, guideIntro)
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## This instance")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Fact | Value |")
	fmt.Fprintln(&b, "|---|---|")
	fmt.Fprintf(&b, "| lease API | %s |\n", in.BaseURL)
	fmt.Fprintf(&b, "| guest service (inside a sandbox) | %s |\n", in.GuestServiceURL)
	fmt.Fprintf(&b, "| model service | %s |\n", in.Inst.ModelService)
	fmt.Fprintf(&b, "| Agent Mail | %s |\n", in.Inst.AgentMail)
	fmt.Fprintf(&b, "| registry | %s |\n", in.Inst.Registry)
	fmt.Fprintf(&b, "| lease API, allowlist form | %s |\n", in.Inst.LeaseAPI)
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Base images")
	fmt.Fprintln(&b)
	images := sortedCopy(in.Inst.Images)
	if len(images) == 0 {
		fmt.Fprintln(&b, "A project's `base_image` may be any image this instance has a current")
		fmt.Fprintln(&b, "build of. The catalog is empty right now; ask the owner to build one.")
	} else {
		fmt.Fprintf(&b, "A project's `base_image` may be any image this instance has a current\nbuild of. Today that is: %s.\n", strings.Join(images, ", "))
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## hive.yaml")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Every field, its type, its default and its rule:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Field | Type | Default | Rule |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, f := range Fields() {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", f.Name, f.Type, f.Default, f.Rule)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "The worker image is derived from `base_image` as `<base>-worker`, and the")
	fmt.Fprintln(&b, "network allowlist from the repo host, the model service, Agent Mail and")
	fmt.Fprintln(&b, "every `needs:` entry; neither is written in the file.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## needs:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Key | Adds to the allowlist |")
	fmt.Fprintln(&b, "|---|---|")
	for _, n := range Needs() {
		fmt.Fprintf(&b, "| `%s` | %s |\n", n.Key, n.Adds)
	}
	fmt.Fprintf(&b, "\nThe repo host, the model service (%s) and Agent Mail (%s) are always\non the allowlist.\n", in.Inst.ModelService, in.Inst.AgentMail)
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Routes")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Route | Auth | What |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, r := range in.Routes {
		fmt.Fprintf(&b, "| `%s %s` | %s | %s |\n", r.Method, r.Path, r.Auth, r.What)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## The checks")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Enlisting has to pass every check, in this order:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| # | Check | Verifies |")
	fmt.Fprintln(&b, "|---|---|---|")
	for i, c := range CheckDescriptions() {
		fmt.Fprintf(&b, "| %d | %s | %s |\n", i+1, c.Name, c.Verifies)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "`POST /hive/check` runs them against a submitted hive.yaml and reports")
	fmt.Fprintln(&b, "each one's outcome plus the exact remedy for the first failure.")
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "Next: %s\n", GuideNext(in.BaseURL))
	return b.String()
}

// guideJSON is the JSON form of the guide: the same values the Markdown
// renders, shaped for a machine reader.
type guideJSON struct {
	What         string      `json:"what"`
	Instance     Instance    `json:"instance"`
	BaseURL      string      `json:"base_url"`
	GuestService string      `json:"guest_service_url"`
	Fields       []Field     `json:"fields"`
	Needs        []Need      `json:"needs"`
	Routes       []Route     `json:"routes"`
	Checks       []CheckDesc `json:"checks"`
	Next         string      `json:"next"`
}

// GuideJSON renders the guide's values as a JSON-marshalable object: the
// same content as Guide, for a caller that asks for
// Accept: application/json.
func GuideJSON(in GuideInput) any {
	in.Inst.Images = sortedCopy(in.Inst.Images)
	return guideJSON{
		What:         guideIntro,
		Instance:     in.Inst,
		BaseURL:      in.BaseURL,
		GuestService: in.GuestServiceURL,
		Fields:       Fields(),
		Needs:        Needs(),
		Routes:       in.Routes,
		Checks:       CheckDescriptions(),
		Next:         GuideNext(in.BaseURL),
	}
}

// GuideNext is the guide's single Next: line.
func GuideNext(baseURL string) string {
	return fmt.Sprintf("write .spoond/hive.yaml, then POST it to %s/hive/check", baseURL)
}

// sortedCopy returns a sorted copy of names.
func sortedCopy(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
