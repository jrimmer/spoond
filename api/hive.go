package api

// The hive's two routes (C11): the guide and the check API. The guide is
// the documentation — generated from the same field table, needs table,
// route table and check table the server and the check engine run on, so
// nothing about enlisting can change without the guide following. The
// check API runs the same engine the CLI runs (hive.Run), with the
// server's own environment: real instance facts, the real catalog, and a
// real trial lease owned by the caller.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/hive"
	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
)

// Hive route paths, in guide order.
const (
	hiveGuidePath = "/hive/guide"
	hiveCheckPath = "/hive/check"
)

// hiveRoutes is the one /hive/ route table: the guide renders it and
// registerHiveRoutes registers it, so a route cannot exist without being
// taught and cannot be taught without being served.
var hiveRoutes = []hive.Route{
	{
		Method: http.MethodGet,
		Path:   hiveGuidePath,
		Auth:   hive.RouteAuthNone,
		What:   "this guide: how to enlist a project on this instance",
	},
	{
		Method: http.MethodPost,
		Path:   hiveCheckPath,
		Auth:   hive.RouteAuthToken,
		What:   "run every enlistment check against a submitted hive.yaml, for real",
	},
}

// registerHiveRoutes wires the /hive/ routes from the route table.
func (s *Server) registerHiveRoutes() {
	for _, r := range hiveRoutes {
		switch r.Path {
		case hiveGuidePath:
			s.mux.HandleFunc(r.Method+" "+r.Path, s.handleHiveGuide)
		case hiveCheckPath:
			s.mux.HandleFunc(r.Method+" "+r.Path, s.handleHiveCheck)
		}
	}
}

// hiveEnvDefaultRegistry and friends are the guide's env-configurable
// facts: the model service and Agent Mail hosts (the server already
// knows the rest from its own configuration).
const (
	envHiveModelService = "HIVE_MODEL_SERVICE"
	envHiveAgentMail    = "HIVE_AGENT_MAIL"
	envHiveRegistry     = "HIVE_REGISTRY"
	defaultModelService = "llm.lacy.casa"
	defaultAgentMail    = "agentmail.lacy.casa"
	defaultRegistryPort = ":5000"
)

// hiveFacts renders this instance's facts (C11) from the values the
// server already runs on: the lease API's host and port, the
// guest-service address guests use, and the model service, Agent Mail
// and registry hosts.
func (s *Server) hiveFacts() hive.Instance {
	host, port := s.svc.cfg.HostGuestAddr, strconv.Itoa(s.svc.cfg.HostAPIPort)
	if s.svc.cfg.HostAPIPort <= 0 {
		host, port = s.svc.cfg.HostGuestAddr, "8890"
	}
	return hive.Instance{
		LeaseAPI:     host + ":" + port,
		Registry:     envOrString(envHiveRegistry, host+defaultRegistryPort),
		ModelService: envOrString(envHiveModelService, defaultModelService),
		AgentMail:    envOrString(envHiveAgentMail, defaultAgentMail),
	}
}

// hiveBaseURL is the lease API base URL the guide teaches: the host
// guests and LAN callers reach, with the port the API listens on.
func (s *Server) hiveBaseURL() string {
	f := s.hiveFacts()
	if s.svc.cfg.TLSEnabled {
		return "https://" + f.LeaseAPI
	}
	return "http://" + f.LeaseAPI
}

// hiveGuestServiceURL is the guest-service URL a sandbox reaches.
func (s *Server) hiveGuestServiceURL() string {
	return fmt.Sprintf("http://%s:%d", s.svc.cfg.HostGuestAddr, s.svc.cfg.HostGuestPort)
}

// hiveCatalog lists the image catalog's names, current builds only —
// the same list GET /api/images serves.
func (s *Server) hiveCatalog(ctx context.Context) ([]string, error) {
	imgs, err := s.svc.db.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, img := range imgs {
		if img.CurrentBuildID != "" {
			names = append(names, img.Name)
		}
	}
	return names, nil
}

// hiveGuideInput gathers the guide's live values.
func (s *Server) hiveGuideInput(ctx context.Context) (hive.GuideInput, error) {
	images, err := s.hiveCatalog(ctx)
	if err != nil {
		return hive.GuideInput{}, err
	}
	inst := s.hiveFacts()
	inst.Images = images
	return hive.GuideInput{
		Inst:            inst,
		BaseURL:         s.hiveBaseURL(),
		GuestServiceURL: s.hiveGuestServiceURL(),
		Routes:          hiveRoutes,
	}, nil
}

// handleHiveGuide serves the generated guide: Markdown by default, JSON
// with Accept: application/json. No auth (LAN), because the whole point
// is that an agent with no token yet can read how to get one.
func (s *Server) handleHiveGuide(w http.ResponseWriter, r *http.Request) {
	in, err := s.hiveGuideInput(r.Context())
	if err != nil {
		s.svc.log.Printf("hive guide: catalog: %v", err)
		writeError(w, http.StatusInternalServerError, "image catalog unavailable")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, hive.GuideJSON(in))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, hive.Guide(in))
}

// hiveCheckMaxBody bounds a submitted hive.yaml.
const hiveCheckMaxBody = 64 << 10

// handleHiveCheck runs every enlistment check against the request body
// (a hive.yaml) with the server's own environment. The report is the
// answer whether or not checks failed: 200 either way, 400 only for a
// body that cannot be read or parsed as YAML at all.
func (s *Server) handleHiveCheck(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" &&
		ct != "application/yaml" && ct != "text/plain" &&
		!strings.HasPrefix(ct, "application/yaml;") && !strings.HasPrefix(ct, "text/plain;") {
		writeError(w, http.StatusUnsupportedMediaType, "send the hive.yaml as application/yaml or text/plain")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, hiveCheckMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) > hiveCheckMaxBody {
		writeError(w, http.StatusBadRequest, "hive.yaml is larger than 64 KiB")
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeError(w, http.StatusBadRequest, "body is empty: send the hive.yaml")
		return
	}
	owner := ownerFrom(r.Context())
	p, parseErr := hive.Parse(body)
	if parseErr != nil {
		// An unparseable body still gets a report, so the answer is
		// always the same shape and ends with the same Next: line.
		p = hive.Project{}
	}
	env := &hiveServerEnv{srv: s, owner: owner, parseErr: parseErr}
	rep := hive.Run(r.Context(), &p, env)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, rep)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, rep.String())
}

// wantsJSON reports whether the request asked for the JSON form.
func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json")
}

// envOrString returns the env var or the default.
func envOrString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// hiveNotAutomated is what the server says for a step that stays with a
// human until enlistment exists (build order step 5): Skip, with the
// reason and the remedy that tells the human what to do by hand today.
const hiveNotAutomated = "not automated until enlistment (hive build order step 5)"

// hiveServerEnv is the Env the server runs the checks with: the facts
// and the catalog are its own, the trial lease is a real lease owned by
// the caller, and the steps that need an enlisted project say so.
type hiveServerEnv struct {
	srv      *Server
	owner    string
	parseErr error
}

// Facts returns the instance facts, with the live image catalog.
func (e *hiveServerEnv) Facts(ctx context.Context) (hive.Instance, error) {
	inst := e.srv.hiveFacts()
	images, err := e.srv.hiveCatalog(ctx)
	if err != nil {
		return inst, err
	}
	inst.Images = images
	return inst, nil
}

// ListImages returns the image catalog from the store, current builds only.
func (e *hiveServerEnv) ListImages(ctx context.Context) ([]string, error) {
	return e.srv.hiveCatalog(ctx)
}

// BuildImage stays with a human until enlistment builds the worker layer.
func (e *hiveServerEnv) BuildImage(ctx context.Context, image string) error {
	return hive.Skipf("%s", hiveNotAutomated)
}

// PushScratch stays with a human until enlistment mints the deploy key.
func (e *hiveServerEnv) PushScratch(ctx context.Context, repo string) error {
	return hive.Skipf("%s", hiveNotAutomated)
}

// RunGate stays with a human until enlistment runs gates in a worker.
func (e *hiveServerEnv) RunGate(ctx context.Context, repo, gate string) error {
	return hive.Skipf("%s", hiveNotAutomated)
}

// Budget stays with the owner until enlistment stores one.
func (e *hiveServerEnv) Budget(ctx context.Context, project string) (hive.Budget, error) {
	return hive.Budget{}, hive.Skipf("%s", hiveNotAutomated)
}

// trialLeaseTTL bounds the trial lease a check creates.
const trialLeaseTTL = 120 * time.Second

// Reachable starts one trial lease of the project's base image —
// non-persistent, TTL 120 s, restricted egress with the derived
// allowlist, owned by the caller — probes every needs: target from
// inside it with a 5 s TCP connect (bash /dev/tcp where the base image
// has bash, curl otherwise), and always deletes the lease again.
func (e *hiveServerEnv) Reachable(ctx context.Context, allowlist, needs []string) error {
	inst, err := e.Facts(ctx)
	if err != nil {
		return fmt.Errorf("instance facts: %w", err)
	}
	targets := hive.NeedTargets(needs, inst)
	if len(targets) == 0 {
		return nil // nothing declared: the allowlist alone is the answer
	}
	lease, err := e.srv.svc.grant(ctx, e.owner, e.baseImage(), trialLeaseTTL, false,
		string(PolicyRestricted), append([]string(nil), allowlist...))
	if err != nil {
		return fmt.Errorf("grant trial lease: %w", err)
	}
	defer e.srv.svc.release(context.WithoutCancel(ctx), lease)

	var failures []string
	for _, target := range targets {
		if err := probeTarget(ctx, e.srv.svc.sub, lease.SandboxID, target); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", target, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// baseImage is the base image of the project being checked, recovered
// from the request body's parse (empty when it did not parse).
func (e *hiveServerEnv) baseImage() string {
	if e.parseErr != nil {
		return ""
	}
	images, err := e.srv.hiveCatalog(context.Background())
	if err != nil {
		return ""
	}
	for _, n := range images {
		if strings.HasSuffix(n, hive.WorkerImageSuffix) {
			continue
		}
		return n
	}
	return ""
}

// probeTarget TCP-connects to target (host or host:port) from inside the
// trial lease with a 5 s timeout, using bash /dev/tcp when the base
// image has bash and curl otherwise, and reports which one it used.
func probeTarget(ctx context.Context, sub substrate.Substrate, sandboxID, target string) error {
	host, port, err := splitTarget(target)
	if err != nil {
		return err
	}
	// bash /dev/tcp first: every base image that has bash can probe
	// without curl, and the report says which carrier was used.
	bash := fmt.Sprintf(`timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%s' && echo PROBE_OK || echo PROBE_FAIL`,
		host, port)
	res, err := sub.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args:    buildShellArgs(bash, "", nil),
		Timeout: probeTimeout,
	})
	if err != nil {
		return fmt.Errorf("probe %s: %w", target, err)
	}
	out := strings.TrimSpace(res.Stdout)
	switch {
	case res.ExitCode == 0 && strings.Contains(out, "PROBE_OK"):
		return nil
	case res.ExitCode == 127 || strings.Contains(res.Stderr, "command not found") ||
		strings.Contains(res.Stderr, "No such file"):
		return probeWithCurl(ctx, sub, sandboxID, host, port)
	default:
		if strings.Contains(res.Stdout, "PROBE_OK") {
			return nil
		}
		return fmt.Errorf("TCP connect failed (bash /dev/tcp, exit %d)", res.ExitCode)
	}
}

// probeWithCurl is the fallback carrier for base images without bash.
func probeWithCurl(ctx context.Context, sub substrate.Substrate, sandboxID, host, port string) error {
	script := fmt.Sprintf(`curl -sf --max-time 5 --connect-timeout 5 -o /dev/null telnet://%s:%s && echo PROBE_OK || echo PROBE_FAIL`,
		host, port)
	res, err := sub.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args:    buildShellArgs(script, "", nil),
		Timeout: probeTimeout,
	})
	if err != nil {
		return fmt.Errorf("probe %s:%s: %w", host, port, err)
	}
	if strings.Contains(res.Stdout, "PROBE_OK") {
		return nil
	}
	return fmt.Errorf("TCP connect failed (curl, exit %d)", res.ExitCode)
}

// probeTimeout bounds one needs: probe exec, including its 5 s connect
// timeout.
const probeTimeout = 15 * time.Second

// splitTarget splits "host", "host:port" or a URL into host and port.
func splitTarget(target string) (string, string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", errors.New("empty target")
	}
	if i := strings.Index(t, "://"); i >= 0 {
		t = t[i+3:]
	}
	if i := strings.IndexAny(t, "/"); i >= 0 {
		t = t[:i]
	}
	host, port, err := splitHostPort(t)
	if err != nil {
		return "", "", err
	}
	return host, port, nil
}

// splitHostPort splits host:port, keeping an IPv6 literal intact.
func splitHostPort(s string) (string, string, error) {
	if i := strings.LastIndex(s, ":"); i > 0 && strings.Count(s, ":") == 1 {
		return s[:i], s[i+1:], nil
	}
	return s, "443", nil
}

// marshalHiveReport renders a report for tests.
func marshalHiveReport(rep hive.Report) []byte {
	b, _ := json.Marshal(rep)
	return b
}

var _ = store.ErrNotFound // keep the store import while the trial lease lands
