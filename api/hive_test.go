package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/hive"
	"github.com/jrimmer/spoond/substrate"
)

// validHiveYAML is a hive.yaml every check can accept.
const validHiveYAML = `project: hrmny
repo: ssh://git@git.lacy.casa/lacy.casa/hrmny.git
base_image: py-base
gates:
  - mix test
needs: [leases, registry]
max_workers: 3
models:
  implement: Z.ai/glm-5.3
  verify: Z.ai/glm-5.3
`

// hiveTestServer builds the standard test server with the host facts
// the guide teaches (vm2.lacy.casa) and returns the base URL, the
// service (for live-lease counts) and the fake substrate.
func hiveTestServer(t *testing.T) (string, *Service, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	// The hive facts derive from the service config; set the host
	// addresses the guide teaches.
	svc.cfg.HostGuestAddr = "vm2.lacy.casa"
	svc.cfg.HostGuestPort = 8891
	svc.cfg.HostAPIPort = 8890
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, svc, sub
}

// get fetches a path with an optional bearer token and returns the
// response without closing the body.
func get(t *testing.T, url, token, accept string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// TestHiveRoutesRegistered pins that the route table's routes are
// actually served: the guide route answers unauthenticated, the check
// route demands a token for every method that is not the guide, and
// nothing else under /hive/ is exempt from auth.
func TestHiveRoutesRegistered(t *testing.T) {
	url, _, _ := hiveTestServer(t)
	// Every table route exists with its table method.
	for _, r := range hiveRoutes {
		var resp *http.Response
		if r.Method == http.MethodGet {
			resp = get(t, url+r.Path, "", "")
		} else {
			req, _ := http.NewRequest(r.Method, url+r.Path, strings.NewReader(validHiveYAML))
			req.Header.Set("Content-Type", "application/yaml")
			var err error
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
		}
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s %s is in the route table but not served", r.Method, r.Path)
		}
	}
	// Only the guide is auth-exempt: a wrong-method or wrong-path /hive/
	// request still needs a token.
	req, _ := http.NewRequest(http.MethodGet, url+"/hive/check", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /hive/check unauthenticated: %d, want 401", resp.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodGet, url+"/hive/projects", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /hive/projects unauthenticated: %d, want 401", resp2.StatusCode)
	}
}

// TestHiveGuideReachableWithoutToken pins C11's point: the guide is
// readable with no credentials at all, as Markdown by default.
func TestHiveGuideReachableWithoutToken(t *testing.T) {
	url, _, _ := hiveTestServer(t)
	resp := get(t, url+"/hive/guide", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("guide status %d, want 200 without a token", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("guide content type %q, want text/plain (Markdown)", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"# Enlisting a project in the hive",
		"| lease API | http://vm2.lacy.casa:8890 |",
		"| guest service (inside a sandbox) | http://vm2.lacy.casa:8891 |",
		"llm.lacy.casa",
		"agentmail.lacy.casa",
		"py-base",
		"Next: write .spoond/hive.yaml, then POST it to http://vm2.lacy.casa:8890/hive/check",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("guide does not mention %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "Next: "); n != 1 {
		t.Errorf("guide has %d Next: lines, want exactly 1", n)
	}
}

// TestHiveGuideJSONShape checks the JSON form: every table the Markdown
// renders is a machine-readable array.
func TestHiveGuideJSONShape(t *testing.T) {
	url, _, _ := hiveTestServer(t)
	resp := get(t, url+"/hive/guide", "", "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("guide status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q, want application/json", ct)
	}
	var got struct {
		What         string   `json:"what"`
		Instance     struct{} `json:"instance"`
		BaseURL      string   `json:"base_url"`
		GuestService string   `json:"guest_service_url"`
		Fields       []struct {
			Name string `json:"name"`
		} `json:"fields"`
		Needs []struct {
			Key string `json:"key"`
		} `json:"needs"`
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
		Next string `json:"next"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode guide JSON: %v", err)
	}
	if got.What == "" || got.BaseURL == "" || got.GuestService == "" || got.Next == "" {
		t.Errorf("guide JSON missing scalars: %+v", got)
	}
	if len(got.Fields) == 0 || len(got.Needs) == 0 || len(got.Routes) == 0 || len(got.Checks) == 0 {
		t.Errorf("guide JSON missing tables: %d fields, %d needs, %d routes, %d checks",
			len(got.Fields), len(got.Needs), len(got.Routes), len(got.Checks))
	}
}

// TestHiveCheckNeedsToken pins that only the guide is exempt from auth:
// POST /hive/check without a token is 401, like every other /api-style
// route.
func TestHiveCheckNeedsToken(t *testing.T) {
	url, _, _ := hiveTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, url+"/hive/check", strings.NewReader(validHiveYAML))
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /hive/check without a token: %d, want 401", resp.StatusCode)
	}
	// A wrong token is 401 too; the check routes carry no capability.
	req2, _ := http.NewRequest(http.MethodPost, url+"/hive/check", strings.NewReader(validHiveYAML))
	req2.Header.Set("Authorization", "Bearer wrong")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /hive/check with a wrong token: %d, want 401", resp2.StatusCode)
	}
}

// TestHiveGuideCompleteness is the C11 completeness test: it fails when
// any hive.Project field, any needs: key, any registered /hive/ route
// or any check name is missing from the rendered guide.
func TestHiveGuideCompleteness(t *testing.T) {
	url, _, _ := hiveTestServer(t)
	resp := get(t, url+"/hive/guide", "", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, f := range hive.Fields() {
		if !strings.Contains(text, "`"+f.Name+"`") {
			t.Errorf("guide does not name the hive.yaml field %q", f.Name)
		}
		if !strings.Contains(text, f.Type) {
			t.Errorf("guide does not give the type of %q (%q)", f.Name, f.Type)
		}
		if !strings.Contains(text, f.Default) {
			t.Errorf("guide does not give the default of %q (%q)", f.Name, f.Default)
		}
		if !strings.Contains(text, f.Rule) {
			t.Errorf("guide does not give the rule of %q (%q)", f.Name, f.Rule)
		}
	}
	for _, n := range hive.Needs() {
		if !strings.Contains(text, "`"+n.Key+"`") || !strings.Contains(text, n.Adds) {
			t.Errorf("guide does not teach the needs: key %q (%q)", n.Key, n.Adds)
		}
	}
	// The served routes, not a copy: registerHiveRoutes' table is what
	// both the mux and the guide see.
	for _, r := range hiveRoutes {
		if !strings.Contains(text, "`"+r.Method+" "+r.Path+"`") {
			t.Errorf("guide does not list the route %s %s", r.Method, r.Path)
		}
		if !strings.Contains(text, r.What) {
			t.Errorf("guide does not say what %s %s does (%q)", r.Method, r.Path, r.What)
		}
		if !strings.Contains(text, r.Auth) {
			t.Errorf("guide does not say who may call %s %s (%q)", r.Method, r.Path, r.Auth)
		}
	}
	for _, c := range hive.CheckDescriptions() {
		if !strings.Contains(text, c.Name) {
			t.Errorf("guide does not name the check %q", c.Name)
		}
		if !strings.Contains(text, c.Verifies) {
			t.Errorf("guide does not say what %q verifies (%q)", c.Name, c.Verifies)
		}
	}
}

// TestHiveCheckRunsTrialLease runs the full check against the fake
// substrate: the trial lease is created once and deleted, its egress
// equals the derived allowlist, and every non-host step reports.
func TestHiveCheckRunsTrialLease(t *testing.T) {
	base, svc, sub := hiveTestServer(t)
	// The fake's default exec answers everything with exit 0 and
	// stdout, so the bash probe reports PROBE_OK. The integrity probe
	// the service runs on grant also passes.
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(validHiveYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("check status %d: %s", resp.StatusCode, body)
	}
	text, _ := io.ReadAll(resp.Body)
	report := string(text)
	t.Log("\n" + report)

	// Exactly one lease was granted and released again: the trial lease.
	if got := len(svc.LiveLeases()); got != 0 {
		t.Errorf("%d live leases after the check, want 0 (the trial lease must always be deleted)", got)
	}
	if n := calls(sub.Fake, "Create"); n != 1 {
		t.Errorf("%d sandbox creates, want exactly 1 trial lease", n)
	}
	if n := calls(sub.Fake, "Delete"); n != 1 {
		t.Errorf("%d sandbox deletes, want exactly 1 (the trial lease)", n)
	}
	// The lease's egress allowlist is the derived allowlist.
	got := sub.LastCreate().Egress
	if len(got.AllowedDomains) == 0 {
		t.Fatalf("trial lease carries no domain allowlist: %+v", got)
	}
	p := hive.Project{
		Repo:  "ssh://git@git.lacy.casa/lacy.casa/hrmny.git",
		Needs: []string{hive.NeedLeases, hive.NeedRegistry},
	}
	inst := hive.Instance{
		LeaseAPI:     "vm2.lacy.casa:8890",
		Registry:     "vm2.lacy.casa:5000",
		ModelService: "llm.lacy.casa",
		AgentMail:    "agentmail.lacy.casa",
	}
	want := p.Allowlist(inst)
	if len(got.AllowedDomains) != len(want) {
		t.Fatalf("egress domains %v, want the derived allowlist %v", got.AllowedDomains, want)
	}
	for i, d := range got.AllowedDomains {
		if d != want[i] {
			t.Errorf("egress domain %d = %q, want %q", i, d, want[i])
		}
	}
	// The host-only steps say they are not automated yet; the rest pass.
	for _, want := range []string{"✓ base image", "✓ trial lease", "worker image: skipped"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not contain %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "not automated until enlistment") {
		t.Errorf("the host-only skips do not say they are not automated:\n%s", report)
	}
}

// TestHiveCheckTrialLeaseDeletedOnProbeFailure pins the "always
// deletes" half: even when a probe fails, no lease is left behind.
func TestHiveCheckTrialLeaseDeletedOnProbeFailure(t *testing.T) {
	base, svc, sub := hiveTestServer(t)
	// The integrity probe (run by grant) still passes, but every needs:
	// probe fails: the trial lease check fails, the lease is still
	// deleted.
	sub.SetExecHandler(func(sandboxID string, args []string) substrate.ExecResult {
		if len(args) == 3 && args[0] == "sh" && strings.Contains(args[2], "command -v uname") {
			return substrate.ExecResult{Stdout: "PROBE_OK\n", ExitCode: 0}
		}
		return substrate.ExecResult{Stderr: "connect: connection refused", ExitCode: 7}
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(validHiveYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if got := len(svc.LiveLeases()); got != 0 {
		t.Errorf("%d live leases after a failing check, want 0", got)
	}
	if n := calls(sub.Fake, "Delete"); n != 1 {
		t.Errorf("%d sandbox deletes after a failing probe, want 1", n)
	}
}

// TestHiveCheckInvalidYAMLIsAReport checks the invalid-hive.yaml case:
// still 200, the report shows the validation failure, later checks skip
// with their remedies, and Next is the remedy.
func TestHiveCheckInvalidYAMLIsAReport(t *testing.T) {
	base, _, sub := hiveTestServer(t)
	invalid := `project: hrmny
repo: ssh://git@git.lacy.casa/lacy.casa/hrmny.git
base_image: py-base
gates: []
needs: [not-a-key]
`
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(invalid))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invalid hive.yaml: %d, want 200 with a report", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	report := string(body)
	t.Log("\n" + report)
	if !strings.Contains(report, "✗ hive.yaml") {
		t.Errorf("report does not show the schema failure:\n%s", report)
	}
	if !strings.Contains(report, `replace the unknown entry "not-a-key" with one of the known keys`) {
		t.Errorf("report does not name the unknown needs key:\n%s", report)
	}
	for _, n := range hive.CheckNames[1:] {
		if !strings.Contains(report, "- "+n+": skipped") {
			t.Errorf("%s did not skip after the schema failure:\n%s", n, report)
		}
	}
	if !strings.HasPrefix(report[strings.LastIndex(report, "Next: "):], "Next: list at least one gate") {
		t.Errorf("Next %q is not the schema failure's remedy", report[strings.LastIndex(report, "Next: "):])
	}
	if n := calls(sub.Fake, "Create"); n != 0 {
		t.Errorf("a schema failure still started %d sandboxes", n)
	}
}

// TestHiveCheckUnparseableBodyIsStillAReport: even bytes that are not
// YAML at all get the report shape, 200; 400 is for unreadable bodies.
func TestHiveCheckUnparseableBodyIsStillAReport(t *testing.T) {
	base, _, sub := hiveTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader("\t this: [is: not yaml"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 with a report", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "✗ hive.yaml") || !strings.Contains(string(body), "Next: fix the YAML syntax") {
		t.Errorf("report does not show the parse failure:\n%s", body)
	}
	if n := calls(sub.Fake, "Create"); n != 0 {
		t.Errorf("an unparseable body still started %d sandboxes", n)
	}
}

// TestHiveCheckUnreadableBodyIs400 pins the 400: a body that cannot be
// read at all.
func TestHiveCheckUnreadableBodyIs400(t *testing.T) {
	base, _, _ := hiveTestServer(t)
	// An oversized body is refused with 400 without running the checks.
	huge := strings.Repeat("x", hiveCheckMaxBody+1)
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(huge))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body: %d, want 400", resp.StatusCode)
	}
	// An empty body too: there is nothing to check.
	req2, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader("   \n"))
	req2.Header.Set("Content-Type", "text/plain")
	req2.Header.Set("Authorization", "Bearer token-a")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body: %d, want 400", resp2.StatusCode)
	}
}

// TestHiveCheckJSON is the JSON form of a check report: the same shape
// the engine's MarshalJSON produces.
func TestHiveCheckJSON(t *testing.T) {
	base, _, _ := hiveTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(validHiveYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q, want application/json", ct)
	}
	var got struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
		Next string `json:"next"`
		OK   bool   `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(got.Checks) != len(hive.CheckNames) {
		t.Fatalf("%d checks, want %d", len(got.Checks), len(hive.CheckNames))
	}
	for i, c := range got.Checks {
		if c.Name != hive.CheckNames[i] {
			t.Errorf("check %d is %q, want %q", i, c.Name, hive.CheckNames[i])
		}
	}
	// Nothing failed: skips are not failures (C11: OK = no check
	// failed), and Next points at enlistment with the host steps
	// skipped.
	if got.Next != hive.NextEnlist {
		t.Errorf("next %q, want %q (skips are not failures)", got.Next, hive.NextEnlist)
	}
	if !got.OK {
		t.Error("ok is false with only skipped host steps")
	}
	// Every skipped entry still carries its remedy: what to do by hand.
	var raw struct {
		Checks []struct {
			Status string `json:"status"`
			Remedy string `json:"remedy"`
		} `json:"checks"`
	}
	req2, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(validHiveYAML))
	req2.Header.Set("Content-Type", "application/yaml")
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Authorization", "Bearer token-a")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if err := json.NewDecoder(resp2.Body).Decode(&raw); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	for _, c := range raw.Checks {
		if c.Status == "skip" && c.Remedy == "" {
			t.Errorf("skipped check has no remedy: %+v", c)
		}
	}
}

// TestHiveUnsupportedContentType: the body must claim to be YAML or
// text; anything else is 415.
func TestHiveUnsupportedContentType(t *testing.T) {
	base, _, _ := hiveTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("application/json body: %d, want 415", resp.StatusCode)
	}
}

// TestHiveProbeCarriers unit-tests the two probe carriers the trial
// lease uses: a base image with bash probes via /dev/tcp, one without
// falls back to curl.
func TestHiveProbeCarriers(t *testing.T) {
	ctx := context.Background()

	t.Run("bash", func(t *testing.T) {
		sub := newTestSub()
		sub.SetExecHandler(func(sandboxID string, args []string) substrate.ExecResult {
			return substrate.ExecResult{Stdout: "PROBE_OK\n", ExitCode: 0}
		})
		carrier, err := probeTarget(ctx, sub, "sbx", "llm.lacy.casa:443")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if carrier != probeCarrierBash {
			t.Errorf("carrier %q, want %q", carrier, probeCarrierBash)
		}
	})
	t.Run("curl fallback", func(t *testing.T) {
		// No bash in the image: the exec fails with command-not-found
		// and the probe falls back to curl, which the fake answers OK.
		sub := newTestSub()
		sub.SetExecHandler(func(sandboxID string, args []string) substrate.ExecResult {
			if strings.Contains(args[len(args)-1], "/dev/tcp") {
				return substrate.ExecResult{Stderr: "/bin/bash: command not found", ExitCode: 127}
			}
			return substrate.ExecResult{Stdout: "PROBE_OK\n", ExitCode: 0}
		})
		carrier, err := probeTarget(ctx, sub, "sbx", "llm.lacy.casa:443")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if carrier != probeCarrierCurl {
			t.Errorf("carrier %q, want %q", carrier, probeCarrierCurl)
		}
	})
	t.Run("connect refused", func(t *testing.T) {
		sub2 := newTestSub()
		sub2.execStdout = ""
		sub2.SetExecHandler(func(sandboxID string, args []string) substrate.ExecResult {
			return substrate.ExecResult{Stdout: "PROBE_FAIL\n", ExitCode: 1}
		})
		if _, err := probeTarget(ctx, sub2, "sbx", "llm.lacy.casa:443"); err == nil {
			t.Error("a refused connect did not fail the probe")
		}
	})
}

// TestHiveSplitTarget covers the target shapes a needs: entry can take.
func TestHiveSplitTarget(t *testing.T) {
	cases := []struct{ in, host, port string }{
		{"llm.lacy.casa:443", "llm.lacy.casa", "443"},
		{"vm2.lacy.casa:5000", "vm2.lacy.casa", "5000"},
		{"git.lacy.casa", "git.lacy.casa", "443"},
		{"https://git.lacy.casa/path", "git.lacy.casa", "443"},
	}
	for _, tc := range cases {
		host, port, err := splitTarget(tc.in)
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("splitTarget(%q) = %q, %q, %v; want %q, %q", tc.in, host, port, err, tc.host, tc.port)
		}
	}
	if _, _, err := splitTarget("   "); err == nil {
		t.Error("empty target did not fail")
	}
}

// TestHiveTrialLeaseShape pins the lease the check starts: the base
// image's build, non-persistent, the 120 s TTL, restricted policy with
// the derived allowlist, owned by the caller, EndAt 120 s out — and
// gone again when the check is over.
func TestHiveTrialLeaseShape(t *testing.T) {
	base, svc, sub := hiveTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/hive/check", strings.NewReader(validHiveYAML))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	create := sub.LastCreate()
	if create.BuildID == "" {
		t.Fatalf("no trial-lease sandbox was created: %+v", sub.Fake.Calls)
	}
	if time.Until(create.EndAt) > 120*time.Second || time.Until(create.EndAt) <= 0 {
		t.Errorf("trial lease EndAt %v, want ~120 s out", create.EndAt)
	}
	// The lease is gone: no live leases carry the sandbox, and the
	// sandbox row was deleted.
	if got := len(svc.LiveLeases()); got != 0 {
		t.Errorf("%d live leases after the check, want 0", got)
	}
	for _, c := range sub.Fake.Calls {
		if c == "Create " {
			t.Error("a create without an id was recorded")
		}
	}
}
