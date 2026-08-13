//go:build linux

// Command forkd-sshd-gateway is the interactive access point for forkd
// sandboxes: `ssh <lease-id>@sandbox.lacy.casa`. It authenticates the
// caller with a public key, resolves the lease id to a running sandbox,
// enters the sandbox's network namespace, and transparently relays an
// SSH session to the sshd inside the VM (dev-base runs sshd + tmux).
//
// The lease id in the username is the capability: it is unguessable
// (128-bit random). The gateway accepts any of the configured client
// keys; the sandbox sshd then authenticates the nested connection with
// the gateway's own key (baked into dev-base authorized_keys).
package spoondgateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/jrimmer/spoond/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/jrimmer/spoond/identity"
)

// envOr returns the value of env key or def when unset/empty. Used for
// flag defaults so the same knobs are settable via environment
// (FORKD_GATEWAY_HOST, SHELLY_BINARY_URL, LLM_GATEWAY_URL).
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	// flags is the gateway's private FlagSet; it stays scoped to this
	// package so the spoond umbrella binary can link every command
	// without flag-name collisions.
	flags = flag.NewFlagSet("spoond-gateway", flag.ExitOnError)

	// gatewayHost is the public hostname advertised in MOTDs.
	gatewayHost = flags.String("gateway-host", envOr("FORKD_GATEWAY_HOST", "sandbox.lacy.casa"), "public hostname advertised in MOTDs")
	listenAddr  = flags.String("listen", ":2222", "listen address")
	hostKeyPath = flags.String("host-key", "/etc/spoond-gateway/ssh_host_ed25519_key", "path to SSH host key (generated if missing)")
	backendURL  = flags.String("backend", "https://127.0.0.1:8890", "spoond-backend base URL")
	// backendTok: the spoond-backend consumer token (required, admin-equivalent).
	// Read from --backend-token flag or SPOOND_GATEWAY_TOKEN env so the
	// value never needs to appear in ExecStart (security review #37 rescan
	// F7: /proc/<pid>/cmdline must not expose the token).
	backendTok = flags.String("backend-token", envOr("SPOOND_GATEWAY_TOKEN", ""), "spoond-backend consumer token (required; or $SPOOND_GATEWAY_TOKEN)")
	// DEPRECATED/ignored (security review #37 rescan F7): the gateway
	// must NOT forward the bootstrap token — bootstrap is an operator
	// action against the backend directly. The flag is accepted so
	// existing units keep working after upgrade.
	bootstrapTok = flags.String("bootstrap-token", "", "DEPRECATED: ignored; bootstrap via direct backend API call")
	clientKeys   = flags.String("client-keys", "", "comma-separated paths to authorized client public keys, or a directory scanned for *.pub files")
	// gatewayKeyPath is the identity the gateway uses to connect INTO
	// sandboxes. Its public half is baked into dev-base authorized_keys.
	gatewayKeyPath = flags.String("gateway-key", "/etc/spoond-gateway/gateway_ed25519", "gateway identity key for nested connections")
	// shellyBinaryURL is where the `shelly` ctl verb fetches the agent
	// binary from inside the sandbox (host-side asset server on the
	// plain-HTTP proxy listener; guests reach it via forkd-br0).
	shellyBinaryURL = flags.String("shelly-binary-url", envOr("SHELLY_BINARY_URL", "http://10.43.0.1:8891/assets/shelley"), "URL the sandbox fetches the shelley binary from")
	// llmGatewayURL is the per-lease LLM gateway base the shelley agent
	// is pointed at (host-side proxy listener; guests reach it via
	// forkd-br0). The lease id is appended.
	llmGatewayURL = flags.String("llm-gateway-url", envOr("LLM_GATEWAY_URL", "http://10.43.0.1:8891/llm/"), "base URL of the per-lease LLM gateway (lease id appended)")
	// shellyModel is the default model id written into shelley.json. It
	// must be an id the LLM gateway's LLM_MODEL_MAP understands (the
	// exe.dev catalog id, not the upstream id).
	shellyModel = flags.String("shelly-model", "gpt-oss-20b-fireworks", "default model id for the shelley agent")

	// sshImages is the set of image tags that have sshd installed and
	// can therefore support interactive SSH sessions. CI images (go-base,
	// py-base, rust-base, etc.) typically don't have sshd. Defaults to
	// dev-base only; operators can add more via GATEWAY_SSH_IMAGES.
	sshImages     = flags.String("ssh-images", envOr("GATEWAY_SSH_IMAGES", "dev-base"), "comma-separated image tags that support interactive SSH (have sshd)")
	metricsListen = flags.String("metrics-listen", envOr("GATEWAY_METRICS_LISTEN", ""), "address for /metrics endpoint (empty = disabled)")

	// extraImageAliases allows operators to add or override short-name
	// → full-tag aliases without code changes. Format: short=full,short=full.
	extraImageAliases = flags.String("image-aliases", envOr("GATEWAY_IMAGE_ALIASES", ""), "comma-separated short=full image aliases (e.g. rust=rust-base,js=js-base)")
)

type endpoint struct {
	ForkdID   string `json:"forkd_id"`
	Netns     string `json:"netns"`
	GuestAddr string `json:"guest_addr"`
	Image     string `json:"image"`
}

func Main(args []string) int {
	flags.Parse(args)

	// Start metrics endpoint (issue #20).
	var gwMetrics *metrics.GatewayMetrics
	if *metricsListen != "" {
		gwMetrics = metrics.NewGatewayMetrics()
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.HandlerFor(gwMetrics.Registry, promhttp.HandlerOpts{}))
			log.Printf("gateway metrics on %s", *metricsListen)
			log.Fatal(http.ListenAndServe(*metricsListen, mux))
		}()
	}
	if *backendTok == "" {
		log.Fatal("--backend-token is required")
	}

	// Parse the set of images that support interactive SSH (have sshd
	// installed). Default is dev-base only; operators can add more via
	// GATEWAY_SSH_IMAGES=dev-base,rust-base,...
	sshImageSet = make(map[string]bool)
	for _, img := range strings.Split(*sshImages, ",") {
		img = strings.TrimSpace(img)
		if img != "" {
			sshImageSet[img] = true
		}
	}

	// Parse extra image aliases from GATEWAY_IMAGE_ALIASES (format:
	// short=full,short=full). These are merged into the hardcoded
	// imageAliases map so operators can add new short names without
	// code changes.
	if *extraImageAliases != "" {
		for _, pair := range strings.Split(*extraImageAliases, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			short, full, ok := strings.Cut(pair, "=")
			if !ok || short == "" || full == "" {
				log.Printf("warning: ignoring malformed image alias %q (expected short=full)", pair)
				continue
			}
			imageAliases[strings.TrimSpace(short)] = strings.TrimSpace(full)
			log.Printf("image alias: %s -> %s", short, full)
		}
	}

	hostKey := loadOrGenerateHostKey(*hostKeyPath)
	gatewayKey := loadOrGenerateKey(*gatewayKeyPath)

	allowed, err := loadAuthorizedKeys(*clientKeys)
	if err != nil {
		log.Fatalf("client keys: %v", err)
	}

	// Identity-store authority (security review #37 H1): when the
	// backend has an identity store, key resolution there is the ONLY
	// gate — the local allowlist is a legacy single-user fallback and
	// must not silently admit keys that the store doesn't know (which
	// would also leave them unscoped after `ssh-key rm`). Probe once at
	// startup; the store's presence doesn't change at runtime.
	identityAuthoritative := false
	if *backendURL != "" && *backendTok != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		b, berr := backendJSON(ctx, http.MethodGet, "/api/identity-status", nil)
		cancel()
		if berr == nil {
			var st struct {
				IdentityStore bool `json:"identity_store"`
			}
			if json.Unmarshal(b, &st) == nil {
				identityAuthoritative = st.IdentityStore
				log.Printf("identity store %v (key resolution %s)",
					identityAuthoritative, map[bool]string{true: "authoritative", false: "local allowlist only"}[identityAuthoritative])
			}
		}
		if identityAuthoritative {
			allowed = nil // local allowlist must not bypass the store
		}
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if len(allowed) == 0 && !identityAuthoritative {
				if gwMetrics != nil {
					gwMetrics.AuthFailures.WithLabelValues("no_keys").Inc()
				}
				return nil, fmt.Errorf("no client keys configured")
			}
			// Accept any key from the allowed set (backward compatible);
			// the lease id in the username is the real capability.
			for _, pk := range allowed {
				if string(pk.Marshal()) == string(key.Marshal()) {
					perms := &ssh.Permissions{
						Extensions: map[string]string{
							"forkd-key-id": fmt.Sprintf("%s %s", ssh.FingerprintSHA256(pk), pk.Type()),
						},
					}
					// When the backend has an identity store, resolve the
					// key to a user id so `ctl whoami` and future
					// owner-scoping can attribute the connection (T1).
					userID, userName := resolveKeyUser(key)
					if userID != "" {
						perms.Extensions["forkd-user-id"] = userID
						perms.Extensions["forkd-user-name"] = userName
					}
					return perms, nil
				}
			}
			// Identity-authoritative mode: the store must know the key.
			if identityAuthoritative {
				userID, userName := resolveKeyUser(key)
				if userID == "" {
					if gwMetrics != nil {
						gwMetrics.AuthFailures.WithLabelValues("identity_not_found").Inc()
					}
					return nil, fmt.Errorf("unknown client key (not in identity store)")
				}
				perms := &ssh.Permissions{
					Extensions: map[string]string{
						"forkd-key-id":    fmt.Sprintf("%s %s", ssh.FingerprintSHA256(key), key.Type()),
						"forkd-user-id":   userID,
						"forkd-user-name": userName,
					},
				}
				return perms, nil
			}
			if gwMetrics != nil {
				gwMetrics.AuthFailures.WithLabelValues("unknown_key").Inc()
			}
			return nil, fmt.Errorf("unknown client key")
		},
	}
	config.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}
	log.Printf("spoond-sshd-gateway listening on %s", *listenAddr)

	// Connection counter goroutine: wrap the accept loop to count connections.
	// The actual accept loop is below; we instrument it inline.

	for {
		conn, err := ln.Accept()
		if err == nil && gwMetrics != nil {
			gwMetrics.ConnectionsTotal.Inc()
		}
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn, config, gatewayKey)
	}
}

func handleConn(conn net.Conn, config *ssh.ServerConfig, gatewayKey ssh.Signer) {
	defer conn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Printf("handshake: %v", err)
		return
	}
	defer sconn.Close()
	user := sconn.User()
	log.Printf("conn: user=%s addr=%s", user, sconn.RemoteAddr())

	// The authenticated key identity (from PublicKeyCallback) rides in
	// the connection permissions, for `ctl whoami`.
	keyID := ""
	userID := ""
	userName := ""
	if sconn.Permissions != nil && sconn.Permissions.Extensions != nil {
		keyID = sconn.Permissions.Extensions["forkd-key-id"]
		userID = sconn.Permissions.Extensions["forkd-user-id"]
		userName = sconn.Permissions.Extensions["forkd-user-name"]
	}
	// All backend calls from this connection run owner-scoped as the SSH
	// user (U6/T5); the gateway impersonates via X-Spoond-User-Id.
	gwCtx := withGatewayUser(context.Background(), userID)

	go ssh.DiscardRequests(reqs)

	// The control plane is a reserved username: `ssh ctl@... "cmd"`.
	// Exec requests become API calls (new/ls/rm/keepalive/cp) and the
	// response is JSON on stdout — no sandbox is dialed.
	if user == "ctl" {
		handleControlPlane(chans, gatewayKey, keyID, userID, userName)
		return
	}

	// Resolve the target: a 32-hex lease id attaches an existing sandbox;
	// a friendly name resolves to its lease; new[-<image>] auto-creates a
	// persistent one (SSH-as-API).
	leaseID := user
	motd := ""
	// Reconnect hint, shown on create AND attach so the user can always
	// get back. Port comes from the listen address (default :2222).
	_, gwPort, _ := net.SplitHostPort(*listenAddr)
	if gwPort == "" {
		gwPort = "22"
	}
	reconnect := func(id string) string {
		return fmt.Sprintf("Reconnect: ssh %s@%s -p %s", id, *gatewayHost, gwPort)
	}
	if !isLeaseID(user) {
		// Try a friendly name first (assigned via `ctl tag <id> <name>`).
		// Anything that's not a lease id and not a new-* create verb is a
		// candidate name; createSandbox rejects unknown verbs below.
		if !strings.HasPrefix(user, "new") {
			if id, ok := resolveName(gwCtx, user); ok {
				leaseID = id
				user = id // keep motd generic below
			}
		}
	}
	if !isLeaseID(user) {
		created, img, err := createSandbox(gwCtx, user)
		if err != nil {
			errMsg := "spoond: " + err.Error() + "\n"
			log.Printf("create for %q failed: %v", user, err)
			// Deliver the error on the first session channel.
			for nc := range chans {
				if nc.ChannelType() != "session" {
					nc.Reject(ssh.UnknownChannelType, "only session channels supported")
					continue
				}
				ch, _, _ := nc.Accept()
				ch.Write([]byte(errMsg))
				ch.Close()
				return
			}
			return
		}
		leaseID = created
		motd = fmt.Sprintf("spoond: created sandbox %s (%s) — tmux 'dev' attached. Detach: Ctrl-b d. %s\n",
			created, img, reconnect(created))
		log.Printf("created sandbox %s (%s) for user %q", created, img, user)
	} else {
		// Attaching to an existing lease: show the id in the tmux footer
		// and print the reconnect hint when the session ends, same as
		// create — the id is just as easy to forget on reconnect.
		motd = fmt.Sprintf("spoond: attached to sandbox %s — tmux 'dev' attached. Detach: Ctrl-b d. %s\n",
			leaseID, reconnect(leaseID))
	}

	// Security review #37 rescan F8: dial with the USER-scoped context,
	// not Background — resolveEndpoint/restartSSHD must run as the SSH
	// user (X-Spoond-User-Id) or attach would resolve as the gateway
	// service identity: user-owned leases would 404 (broken attach in
	// store mode) and gateway-owned leases would be attachable by any
	// user without an ownership check.
	client, err := dialSandbox(gwCtx, leaseID, gatewayKey)
	if err != nil {
		log.Printf("dial sandbox for %s: %v", leaseID, err)
		// Tell the client what happened with a session-level error.
		for nc := range chans {
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.UnknownChannelType, "only session channels supported")
				continue
			}
			ch, _, _ := nc.Accept()
			fmt.Fprintf(ch, "spoond: cannot reach sandbox %s: %v\n", leaseID, err)
			ch.Close()
			return
		}
		return
	}
	defer client.Close()

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		go handleSession(newChan, client, motd)
	}
}

// isLeaseID reports whether s is a 32-char lowercase hex lease id.
func isLeaseID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// imageAliases maps SSH username image names to backend snapshot tags.
var imageAliases = map[string]string{
	"dev":    "dev-base",
	"go":     "go-base",
	"py":     "py-base",
	"python": "py-base",
	"elixir": "elixir-base",
	"llm":    "llm-review",
	"base":   "dev-base",
}

// sshImageSet is populated from the --ssh-images flag at startup.
var sshImageSet map[string]bool

// fetchKnownImages queries the backend's /api/images endpoint and returns
// the list of known image tags. This is used to validate full image names
// (e.g. "rust-base") that aren't in the imageAliases short-name map.
func fetchKnownImages(ctx context.Context) ([]string, error) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/images", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse /api/images: %w", err)
	}
	return resp.Images, nil
}

// sshImageTags returns a sorted slice of image tags that support
// interactive SSH, for use in error messages.
func sshImageTags() []string {
	tags := make([]string, 0, len(sshImageSet))
	for tag := range sshImageSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// createSandbox parses an SSH username of the form new[-<image>] and
// creates a persistent sandbox in the backend. Returns the new lease id
// and the resolved image tag.
func createSandbox(ctx context.Context, user string) (string, string, error) {
	image := "dev-base"
	alias := ""
	if user != "new" {
		rest, ok := strings.CutPrefix(user, "new-")
		if !ok || rest == "" {
			return "", "", fmt.Errorf("unknown command %q — use a lease id or new[-<image>] (new, new-dev, new-go, new-py, new-elixir, new-llm, or new-<full-tag>)", user)
		}
		alias = rest
	}
	if alias != "" {
		tag, ok := imageAliases[alias]
		if !ok {
			// Not a known short alias — try matching as a full image
			// tag against the backend's known images. This lets new
			// images (e.g. rust-base) be used immediately without
			// adding a code-level alias.
			known, err := fetchKnownImages(ctx)
			if err != nil {
				return "", "", fmt.Errorf("cannot verify image %q (backend /api/images unavailable: %v) — try a known short name: dev, go, py, elixir, llm", alias, err)
			}
			found := false
			for _, k := range known {
				if k == alias {
					image = alias
					found = true
					break
				}
			}
			if !found {
				sort.Strings(known)
				return "", "", fmt.Errorf("unknown image %q — try a short name (dev, go, py, elixir, llm) or a known tag: %s", alias, strings.Join(known, ", "))
			}
		} else {
			image = tag
		}
	}

	// Interactive SSH requires an image with sshd. Most CI images (go,
	// py, elixir, rust, etc.) don't have sshd — they're for API exec
	// workflows, not interactive shells. Reject before creating so we
	// don't orphan a sandbox.
	if !sshImageSet[image] {
		return "", "", fmt.Errorf("image %q is not an interactive SSH image (no sshd) — interactive SSH requires one of: %s. Use the backend API for CI images.", image, strings.Join(sshImageTags(), ", "))
	}

	payload, err := json.Marshal(map[string]any{
		"image":      image,
		"persistent": true,
		"ttl":        3600,
	})
	if err != nil {
		return "", "", err
	}
	b, err := backendJSONRetry(ctx, http.MethodPost, "/api/sandboxes", payload)
	if err != nil {
		return "", "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &created); err != nil {
		return "", "", fmt.Errorf("create response: %w", err)
	}
	if created.ID == "" {
		return "", "", fmt.Errorf("create response missing id: %s", strings.TrimSpace(string(b)))
	}
	return created.ID, image, nil
}

// dialSandbox resolves the lease to its netns+address and opens a nested
// SSH client connection to the sandbox's sshd using the gateway key.
func dialSandbox(ctx context.Context, leaseID string, gatewayKey ssh.Signer) (*ssh.Client, error) {
	ep, err := resolveEndpoint(ctx, leaseID)
	if err != nil {
		return nil, err
	}

	// Restart sshd inside the sandbox. Firecracker restore carries the
	// process table over, but the pre-snapshot sshd's listening socket is
	// dead in the restored netns (the guest kernel re-initializes its
	// network stack on restore). A fresh sshd binds cleanly. We reach the
	// agent via the backend exec API — the agent socket survives restore.
	if err := restartSSHD(ctx, leaseID); err != nil {
		log.Printf("sshd restart for %s: %v", leaseID, err)
	}

	host, port, err := net.SplitHostPort(ep.GuestAddr)
	if err != nil {
		// GuestAddr may be host:port for the agent; sshd is on port 22.
		host = ep.GuestAddr
		port = "22"
	}
	if port == "8888" {
		port = "22"
	}
	target := net.JoinHostPort(host, port)

	// Enter the sandbox's netns on this thread, dial, then return to the
	// default netns for the relay. setns requires a locked thread.
	var dialed net.Conn
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		nsPath := filepath.Join("/var/run/netns", ep.Netns)
		f, err := os.Open(nsPath)
		if err != nil {
			errCh <- fmt.Errorf("open netns %s: %w", nsPath, err)
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("setns %s: %w", ep.Netns, err)
			return
		}
		d, err := net.DialTimeout("tcp", target, 10*time.Second)
		if err != nil {
			errCh <- fmt.Errorf("dial %s in netns %s: %w", target, ep.Netns, err)
			return
		}
		dialed = d
		errCh <- nil
	}()
	if err := <-errCh; err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(gatewayKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // homelab; dev-base host key regenerates per bake
		Timeout:         10 * time.Second,
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(dialed, target, cfg)
	if err != nil {
		dialed.Close()
		return nil, fmt.Errorf("nested ssh to %s: %w", target, err)
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

// handleControlPlane implements the SSH-as-API control plane for the
// reserved `ctl` username. It accepts the first session channel, runs
// the exec command as a forkd API call, writes JSON to the channel and
// closes it. Usage:
//
//	ssh ctl@sandbox.lacy.casa "new [image]"     create a persistent lease
//	ssh ctl@sandbox.lacy.casa "ls"              list leases
//	ssh ctl@sandbox.lacy.casa "rm <lease-id>"   delete a lease
//	ssh ctl@sandbox.lacy.casa "keepalive <id>"  extend a lease
//	ssh ctl@sandbox.lacy.casa "cp <id> [tag]"   clone a sandbox (branch)
//	ssh ctl@sandbox.lacy.casa "help"
func handleControlPlane(chans <-chan ssh.NewChannel, gatewayKey ssh.Signer, keyID, userID, userName string) {
	// Note: ctl command metrics would need gwMetrics passed through the
	// call chain. For now, connection-level metrics are captured here;
	// per-verb metrics require threading gwMetrics through handleControlPlane.
	// This is a follow-up enhancement.
	// All ctl verbs run owner-scoped as the SSH user (U6/T5).
	gwCtx := withGatewayUser(context.Background(), userID)
	// Accept the first session channel; anything else is rejected.
	var ch ssh.Channel
	var reqs <-chan *ssh.Request
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		var err error
		ch, reqs, err = nc.Accept()
		if err != nil {
			return
		}
		break
	}
	if ch == nil {
		return
	}
	defer ch.Close()

	// Read the exec request from the channel's request stream.
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		var msg struct {
			Command string
		}
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
			fmt.Fprintf(ch, `{"error":"bad exec payload: %v"}`+"\n", err)
			if req.WantReply {
				req.Reply(false, nil)
			}
			return
		}
		if req.WantReply {
			req.Reply(true, nil)
		}
		out := runControlCommand(gwCtx, msg.Command, gatewayKey, keyID, userID, userName)
		ch.Write([]byte(out + "\n"))
		return
	}
}

// runControlCommand executes one control-plane command and returns the
// JSON-ish response text written to the client.
//
// Default output is human-readable (ticket #27); `--json` anywhere in
// the command opts into raw machine format for scripts/LLM skills.
func runControlCommand(ctx context.Context, cmd string, gatewayKey ssh.Signer, keyID, userID, userName string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return `{"error":"empty command — try new, ls, rm, keepalive, cp, help"}`
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
	case "help", "--help", "-h":
		return "commands: new [dev|go|py|elixir|llm], ls [--json], stat <id> [--json], rm <id>, keepalive <id>, suspend <id>, resume <id>, restart <id>, cp <id> [tag], shelly <id>, tag <id> <name>, comment <id> <text>, whoami, prompt <id> <message>, env ls|new <repo> <pr>|rm <repo> <pr>|id <repo> <pr>, ssh-key ls|add <pubkey> <name>|rm <id>, share add <id> <user> [ssh|http] [ttl]|ls|rm <id> <user> — add --json for raw output"
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
	case "new":
		user := "new"
		if len(fields) > 1 {
			user = "new-" + fields[1]
		}
		id, img, err := createSandbox(ctx, user)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return fmt.Sprintf(`{"id":%q,"image":%q,"created":true}`, id, img)
	case "ls":
		b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		if jsonMode {
			return strings.TrimSpace(string(b))
		}
		return prettySandboxTable(b)
	case "stat":
		if len(fields) < 2 {
			return `{"error":"usage: stat <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes/"+fields[1]+"/stat", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		if jsonMode {
			return strings.TrimSpace(string(b))
		}
		return prettyStat(b)
	case "rm":
		if len(fields) < 2 {
			return `{"error":"usage: rm <lease-id>"}`
		}
		if err := backendJSONErr(ctx, http.MethodDelete, "/api/sandboxes/"+fields[1], nil); err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return fmt.Sprintf(`{"id":%q,"deleted":true}`, fields[1])
	case "keepalive", "ka":
		if len(fields) < 2 {
			return `{"error":"usage: keepalive <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/keepalive", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "suspend":
		if len(fields) < 2 {
			return `{"error":"usage: suspend <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/suspend", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "resume":
		if len(fields) < 2 {
			return `{"error":"usage: resume <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/resume", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "cp", "clone":
		if len(fields) < 2 {
			return `{"error":"usage: cp <lease-id> [tag]"}`
		}
		payload := []byte("{}")
		if len(fields) > 2 {
			p, _ := json.Marshal(map[string]string{"tag": fields[2]})
			payload = p
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/clone", payload)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "shelly", "agent":
		if len(fields) < 2 {
			return `{"error":"usage: shelly <lease-id>"}`
		}
		return runShelly(ctx, fields[1])
	case "restart":
		if len(fields) < 2 {
			return `{"error":"usage: restart <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/restart", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "tag":
		if len(fields) < 3 {
			return `{"error":"usage: tag <lease-id> <name>"}`
		}
		p, _ := json.Marshal(map[string]string{"name": fields[2]})
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/tag", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "comment":
		if len(fields) < 2 {
			return `{"error":"usage: comment <lease-id> [text...] (no text clears)"}`
		}
		text := ""
		if len(fields) > 2 {
			text = strings.Join(fields[2:], " ")
		}
		p, _ := json.Marshal(map[string]string{"comment": text})
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/comment", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "env":
		if len(fields) < 2 {
			return `{"error":"usage: env ls | env new <repo> <pr> [image] | env rm <repo> <pr> | env id <repo> <pr>"}`
		}
		switch fields[1] {
		case "ls":
			b, err := backendJSON(ctx, http.MethodGet, "/api/environments", nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return strings.TrimSpace(string(b))
		case "new":
			if len(fields) < 4 {
				return `{"error":"usage: env new <repo> <pr> [image]"}`
			}
			img := ""
			if len(fields) > 4 {
				img = fields[4]
			}
			p, _ := json.Marshal(map[string]string{"repo": fields[2], "ref": fields[3], "image": img})
			b, err := backendJSON(ctx, http.MethodPost, "/api/environments", p)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return strings.TrimSpace(string(b))
		case "rm":
			if len(fields) < 4 {
				return `{"error":"usage: env rm <repo> <pr>"}`
			}
			path := "/api/environments?repo=" + url.QueryEscape(fields[2]) + "&ref=" + url.QueryEscape(fields[3])
			if err := backendJSONErr(ctx, http.MethodDelete, path, nil); err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return fmt.Sprintf(`{"deleted":true,"repo":%q,"ref":%q}`, fields[2], fields[3])
		case "id":
			if len(fields) < 4 {
				return `{"error":"usage: env id <repo> <pr>"}`
			}
			path := "/api/environments?repo=" + url.QueryEscape(fields[2]) + "&ref=" + url.QueryEscape(fields[3])
			b, err := backendJSON(ctx, http.MethodGet, path, nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			var resp struct {
				Envs []struct {
					SandboxID string `json:"sandbox_id"`
				} `json:"environments"`
			}
			if err := json.Unmarshal(b, &resp); err != nil || len(resp.Envs) == 0 {
				return `{"error":"environment not found"}`
			}
			return fmt.Sprintf(`{"sandbox_id":%q}`, resp.Envs[0].SandboxID)
		default:
			return `{"error":"unknown env subcommand (ls|new|rm|id)"}`
		}
	case "prompt":
		if len(fields) < 3 {
			return `{"error":"usage: prompt <lease-id> <message...>"}`
		}
		msg := strings.Join(fields[2:], " ")
		p, _ := json.Marshal(map[string]string{"message": msg})
		// The in-guest agent can take minutes to reply; backendJSON's
		// default client would drop the response at 10s. Use the
		// long-timeout client for this verb only.
		b, err := backendJSONWith(ctx, backendClientLong(), http.MethodPost, "/api/sandboxes/"+fields[1]+"/prompt", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "ssh-key", "keys":
		// Identity management (T1). ssh-key ls / ssh-key add <pubkey> <name> / ssh-key rm <user-id>
		if len(fields) < 2 {
			return `{"error":"usage: ssh-key ls | ssh-key add <pubkey> <name> | ssh-key rm <user-id>"}`
		}
		sub := fields[1]
		switch sub {
		case "ls":
			b, err := backendJSON(ctx, http.MethodGet, "/api/users", nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			var resp struct {
				Users []struct {
					ID    string   `json:"id"`
					Name  string   `json:"name"`
					Kind  string   `json:"kind"`
					Admin bool     `json:"admin"`
					Keys  []string `json:"fingerprints"`
				} `json:"users"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
				return strings.TrimSpace(string(b))
			}
			if len(resp.Users) == 0 {
				return "no users"
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%-16s %-10s %-6s %s\n", "NAME", "KIND", "ADMIN", "KEYS")
			for _, u := range resp.Users {
				admin := "no"
				if u.Admin {
					admin = "yes"
				}
				keys := ""
				if len(u.Keys) > 0 {
					keys = u.Keys[0]
					if len(u.Keys) > 1 {
						keys += fmt.Sprintf(" (+%d)", len(u.Keys)-1)
					}
				}
				fmt.Fprintf(&sb, "%-16s %-10s %-6s %s\n", u.Name, u.Kind, admin, keys)
			}
			return strings.TrimSuffix(sb.String(), "\n")
		case "add":
			if len(fields) < 4 {
				return `{"error":"usage: ssh-key add <pubkey> <name>"}`
			}
			// Verify + fingerprint the provided public key.
			pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fields[2]))
			if err != nil {
				return fmt.Sprintf(`{"error":"bad public key: %v"}`, err)
			}
			fp := identity.FingerprintSHA256(pub.Marshal())
			p, _ := json.Marshal(map[string]any{
				"name":         fields[3],
				"kind":         "person",
				"fingerprints": []string{fp},
			})
			b, err := backendJSON(ctx, http.MethodPost, "/api/users", p)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			return fmt.Sprintf("added user %q with key %s", fields[3], fp)
		case "rm":
			if len(fields) < 3 {
				return `{"error":"usage: ssh-key rm <user-id>"}`
			}
			if err := backendJSONErr(ctx, http.MethodDelete, "/api/users/"+fields[2], nil); err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return fmt.Sprintf(`{"deleted":%q}`, fields[2])
		default:
			return `{"error":"unknown ssh-key subcommand — ls, add, rm"}`
		}
	case "share":
		// Sharing (T6/#33). share add <lease-id> <user-id> [ssh|http] [ttl] /
		// share ls / share rm <lease-id> <user-id>
		if len(fields) < 2 {
			return `{"error":"usage: share add <lease-id> <user-id> [ssh|http] [ttl] | share ls | share rm <lease-id> <user-id>"}`
		}
		sub := fields[1]
		switch sub {
		case "add":
			if len(fields) < 4 {
				return `{"error":"usage: share add <lease-id> <user-id-or-name> [ssh|http] [ttl]"}`
			}
			mode := "http"
			ttl := 0
			if len(fields) > 4 && (fields[4] == "ssh" || fields[4] == "http") {
				mode = fields[4]
			}
			if len(fields) > 5 {
				if n, err := strconv.Atoi(fields[5]); err == nil {
					ttl = n
				}
			}
			// The grantee may be a user id or a username; usernames
			// resolve via the minimal by-name endpoint (security review
			// #37 C1 — the full directory is admin-only now).
			grantee := fields[3]
			if !strings.HasPrefix(grantee, "u-") {
				b, err := backendJSON(ctx, http.MethodGet, "/api/users/by-name/"+url.PathEscape(grantee), nil)
				if err != nil {
					return fmt.Sprintf(`{"error":"unknown user %q: %v"}`, grantee, err)
				}
				var ru struct {
					User struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"user"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal(b, &ru); err != nil || ru.Error != "" || ru.User.ID == "" {
					return fmt.Sprintf(`{"error":"unknown user %q"}`, grantee)
				}
				grantee = ru.User.ID
			}
			p, _ := json.Marshal(map[string]any{"grantee": grantee, "mode": mode, "ttl": ttl})
			b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[2]+"/share", p)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return strings.TrimSpace(string(b))
		case "ls":
			b, err := backendJSON(ctx, http.MethodGet, "/api/shares", nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			var resp struct {
				Shares []struct {
					LeaseID string `json:"lease_id"`
					Grantee string `json:"grantee"`
					Mode    string `json:"mode"`
					Expires string `json:"expires_at"`
				} `json:"shares"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
				return strings.TrimSpace(string(b))
			}
			if len(resp.Shares) == 0 {
				return "no shares"
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%-36s %-12s %-6s %s\n", "LEASE", "GRANTEE", "MODE", "EXPIRES")
			for _, sh := range resp.Shares {
				exp := "never"
				if sh.Expires != "" {
					exp = sh.Expires
				}
				fmt.Fprintf(&sb, "%-36s %-12s %-6s %s\n", sh.LeaseID, sh.Grantee, sh.Mode, exp)
			}
			return strings.TrimSuffix(sb.String(), "\n")
		case "rm":
			if len(fields) < 4 {
				return `{"error":"usage: share rm <lease-id> <user-id>"}`
			}
			if err := backendJSONErr(ctx, http.MethodDelete, "/api/sandboxes/"+fields[2]+"/share/"+fields[3], nil); err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return fmt.Sprintf(`{"revoked":%q,"grantee":%q}`, fields[2], fields[3])
		default:
			return `{"error":"unknown share subcommand — add, ls, rm"}`
		}
	default:
		return fmt.Sprintf(`{"error":"unknown command %q — try new, ls, rm, keepalive, cp, shelly, restart, tag, prompt, ssh-key, share, help"}`, fields[0])
	}
}

// resolveKeyUser asks the backend identity store which user owns an SSH
// key. Returns empty strings when the backend has no identity store
// (legacy single-user mode) or the key is unknown. The gateway must not
// block SSH auth on this — a slow/missing backend falls back to
// allowlist-only auth.
func resolveKeyUser(key ssh.PublicKey) (userID, userName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fp := identity.FingerprintSHA256(key.Marshal())
	b, err := backendJSON(ctx, http.MethodGet, "/api/users/by-key?fingerprint="+url.QueryEscape(fp), nil)
	if err != nil {
		return "", ""
	}
	var resp struct {
		User struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" || resp.User.ID == "" {
		return "", ""
	}
	return resp.User.ID, resp.User.Name
}

// backendJSONErr is backendJSON for calls that expect no response body;
// it returns only the error (nil on success).
func backendJSONErr(ctx context.Context, method, path string, payload []byte) error {
	_, err := backendJSON(ctx, method, path, payload)
	return err
}

func handleSession(newChan ssh.NewChannel, client *ssh.Client, motd string) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	// Open a session channel on the nested client. dev-base's login hook
	// attaches to (or creates) the tmux session, so a plain shell gets
	// Jason into his tmux automatically.
	sess, err := client.NewSession()
	if err != nil {
		log.Printf("nested session: %v", err)
		ch.Write([]byte("gateway: nested session failed\n"))
		return
	}
	defer sess.Close()

	// Pipes must exist BEFORE Shell()/Start() so the session's internal
	// start() can wire them to the channel.
	stdin, err := sess.StdinPipe()
	if err != nil {
		log.Printf("stdin pipe: %v", err)
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		log.Printf("stdout pipe: %v", err)
		return
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		log.Printf("stderr pipe: %v", err)
		return
	}

	started := make(chan struct{}, 1)
	startErr := make(chan error, 1)
	// motdReady is signaled by the request loop when the nested session
	// actually starts (shell OR exec). It is SEPARATE from `started`
	// because the main flow below also consumes `started` — a single
	// value can only have one receiver, so the MOTD goroutine must not
	// compete for it.
	motdReady := make(chan struct{}, 1)

	// Deliver the MOTD ("created sandbox …") on new auto-created
	// sandboxes. The MOTD itself is written to the channel BEFORE the
	// nested shell starts (above) so it survives in the terminal's
	// scrollback. This goroutine only adds tmux presentation: a
	// transient `display-message` popup plus a persistent `status-right`
	// so the lease id stays visible in-session. Images without tmux are
	// unaffected (the pre-shell write already delivered the MOTD).
	if motd != "" {
		go func() {
			<-motdReady
			// Give the guest's login hook a beat to attach tmux.
			time.Sleep(1500 * time.Millisecond)
			for attempt := 0; attempt < 4; attempt++ {
				s2, err := client.NewSession()
				if err == nil {
					msg := strings.ReplaceAll(motd, "\n", " ")
					// Extract the lease id (first 32-hex token) for the
					// persistent status-right display.
					id := ""
					if m := regexp.MustCompile(`[0-9a-f]{32}`).FindString(msg); m != "" {
						id = m
					}
					// `tmux display-message -t dev` from a second
					// session renders on the attached client's status
					// bar; `set-option status-right` keeps the id
					// visible.
					out, err := s2.Output("tmux display-message -t dev '" + msg + "' 2>/dev/null; tmux set-option -t dev status-right 'id: " + id + "' 2>/dev/null; echo __TMUX_DONE__")
					s2.Close()
					if err == nil && strings.Contains(string(out), "__TMUX_DONE__") {
						return
					}
				}
				time.Sleep(750 * time.Millisecond)
			}
		}()
	}

	// Forward client requests. pty/env/window-change go through as-is;
	// shell/exec START the nested session (SendRequest alone never
	// activates the pipes — the session needs Shell()/Start()).
	go func() {
		for req := range reqs {
			switch req.Type {
			case "pty-req", "env", "window-change", "signal", "subsystem":
				ok, err := sess.SendRequest(req.Type, req.WantReply, req.Payload)
				if err != nil {
					startErr <- err
					return
				}
				if req.WantReply {
					if err := req.Reply(ok, nil); err != nil {
						return
					}
				}
			case "shell":
				err := sess.Shell()
				started <- struct{}{}
				startErr <- err
				motdReady <- struct{}{}
				// Reply to the CLIENT's shell request (OpenSSH blocks
				// until it gets channel success/failure).
				if req.WantReply {
					_ = req.Reply(err == nil, nil)
				}
				return
			case "exec":
				// The exec request payload is a marshaled string
				// (length prefix + padding) — NOT a plain command.
				var msg struct {
					Command string
				}
				if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
					startErr <- fmt.Errorf("bad exec payload: %w", err)
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					return
				}
				log.Printf("session exec: %q", msg.Command)
				err := sess.Start(msg.Command)
				started <- struct{}{}
				startErr <- err
				motdReady <- struct{}{}
				// Reply to the CLIENT's exec request.
				if req.WantReply {
					_ = req.Reply(err == nil, nil)
				}
				return
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()

	// Wait for the nested session to start before relaying stdio.
	select {
	case <-started:
		if err := <-startErr; err != nil {
			log.Printf("nested start: %v", err)
			return
		}
	case <-time.After(30 * time.Second):
		log.Printf("nested session never started")
		return
	}
	// Relay: client channel -> nested stdin, nested stdout/stderr ->
	// client channel. Wait for BOTH output relays: firing on either one
	// alone tears down the channel while the other still has buffered
	// output (banner arrives, exec output gets truncated).
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stdin, ch)
		stdin.Close()
	}()
	go func() {
		_, _ = io.Copy(ch, stdout)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(ch, stderr)
		done <- struct{}{}
	}()

	// Wait for both relays to finish (EOF on each stream).
	<-done
	<-done

	// The nested session has ended (user exited tmux/shell). Now show
	// the MOTD so the lease id is visible after the session — the user
	// asked for the id "on the command line in case they don't take
	// note of it before exiting"; writing it now lands it right where
	// they're looking when the connection closes.
	if motd != "" {
		_, _ = ch.Write([]byte(motd))
	}

	// Forward the nested exit status to the client channel so the ssh
	// client knows the command finished (otherwise it hangs).
	// sess.Wait() returns after the nested session's exit-status arrives.
	if err := sess.Wait(); err != nil {
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct {
				Status uint32
			}{uint32(exitErr.ExitStatus())}))
		}
	}
}

// backendClient returns an HTTP client for our own backend (loopback;
// skips TLS verify since the LAN cert doesn't cover 127.0.0.1).
func backendClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// backendClientLong is used for long-running backend calls (e.g. the
// prompt verb, whose in-guest agent can take minutes to reply). The
// lease exec timeout (240s) bounds the real work; give the client
// enough headroom so the backend response is not dropped mid-flight.
func backendClientLong() *http.Client {
	return &http.Client{
		Timeout: 280 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// backendJSON performs a JSON request against the backend with the
// consumer token and returns the response body (or an error on non-2xx).
// Transient connection errors (refused/reset/timeout — e.g. backend busy
// refilling the warm pool) are retried with backoff; the gateway should
// not drop an SSH session because one loopback request got refused.
// backendJSONRetry is backendJSON with a longer retry window for
// create paths. The backend can briefly be unreachable during its idle
// sweep + restart (systemd RestartSec=3, then pool refill); a 3s
// window drops interactive `new@` sessions. This retries transport
// errors with 1/2/4s backoff (~7s total) — long enough to ride out a
// brief restart blip, short enough that an interactive user isn't left
// hanging. If the backend hasn't come back by then it's down for a real
// restart, and waiting longer won't help. HTTP/validation errors are
// still returned immediately (never retried).
func backendJSONRetry(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var lastErr error
	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		b, err := backendJSONOnceWith(ctx, backendClient(), method, path, body)
		if err == nil {
			return b, nil
		}
		lastErr = err
		var uerr *url.Error
		if !errors.As(err, &uerr) {
			return nil, err
		}
	}
	return nil, lastErr
}

// backendJSON is backendJSONWith for the short-timeout client; used for
// verbs that should fail fast (list, exec, keepalive).
func backendJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return backendJSONWith(ctx, backendClient(), method, path, body)
}

// backendJSONWith is backendJSON with an explicit client (e.g. the
// long-timeout client for slow verbs like prompt).
func backendJSONWith(ctx context.Context, client *http.Client, method, path string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		b, err := backendJSONOnceWith(ctx, client, method, path, body)
		if err == nil {
			return b, nil
		}
		lastErr = err
		// Only retry transient transport failures, not HTTP/validation errors.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func backendJSONOnce(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return backendJSONOnceWith(ctx, backendClient(), method, path, body)
}

// ctxUserIDKey carries the SSH-authenticated user id (U6/T5). The
// backend honors it via X-Spoond-User-Id only when the gateway's own
// service token is used — the gateway never forwards a user-supplied
// token, so impersonation stays trusted.
type ctxUserIDKey struct{}

// withGatewayUser returns a context that makes backendJSON attach the
// X-Spoond-User-Id header, so ctl verbs run owner-scoped as the SSH
// user instead of the gateway service identity.
func withGatewayUser(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxUserIDKey{}, userID)
}

func backendJSONOnceWith(ctx context.Context, client *http.Client, method, path string, body []byte) ([]byte, error) {
	url := strings.TrimRight(*backendURL, "/") + path
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+*backendTok)
	if uid, _ := ctx.Value(ctxUserIDKey{}).(string); uid != "" {
		req.Header.Set("X-Spoond-User-Id", uid)
	}
	// Deliberately NOT forwarding X-Bootstrap-Token (security review
	// #37 rescan F7): the bootstrap token gates the first-user create on
	// a fresh store, and the SSH gateway is the most exposed component.
	// Replaying it unconditionally from here would hand any allowlisted
	// key holder admin on a fresh deployment (gateway starts while the
	// backend is down → allowlist keys authenticate → `ssh-key add`
	// creates the first user as admin). Bootstrap is an operator action
	// via direct API call (curl with the backend's BOOTSTRAP_TOKEN).
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// restartSSHD pkill's and restarts sshd inside the sandbox via the
// backend exec API. The agent socket survives Firecracker restore; the
// pre-snapshot sshd listener does not. Returns a clear error when the
// image has no sshd (CI images are not interactive).
func restartSSHD(ctx context.Context, leaseID string) error {
	cmd := "command -v /usr/sbin/sshd >/dev/null 2>&1 || echo NO_SSHD_BINARY; pkill -x sshd 2>/dev/null; sleep 1; mkdir -p /run/sshd; /usr/sbin/sshd 2>/dev/null; sleep 1; pgrep -x sshd >/dev/null || echo SSHD_NOT_RUNNING"
	payload, err := json.Marshal(map[string]any{"cmd": cmd})
	if err != nil {
		return err
	}
	b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+leaseID+"/exec", payload)
	if err != nil {
		return err
	}
	var out struct {
		Stdout string `json:"stdout"`
	}
	_ = json.Unmarshal(b, &out)
	if strings.Contains(out.Stdout, "NO_SSHD_BINARY") {
		return fmt.Errorf("image has no sshd — interactive SSH requires dev-base (use new or new-dev)")
	}
	if strings.Contains(out.Stdout, "SSHD_NOT_RUNNING") {
		return fmt.Errorf("sshd did not start")
	}
	return nil
}

func resolveEndpoint(ctx context.Context, leaseID string) (*endpoint, error) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes/"+leaseID+"/endpoint", nil)
	if err != nil {
		return nil, err
	}
	var ep endpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// resolveName looks up a friendly lease name and returns its lease id.
func resolveName(ctx context.Context, name string) (string, bool) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/names/"+name, nil)
	if err != nil {
		return "", false
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		return "", false
	}
	return out.ID, true
}

// runShelly implements the `shelly <lease-id>` ctl verb: it bootstraps
// the coding agent inside the lease. The binary is fetched from the
// backend's asset server (plain HTTP on the proxy listener, reachable
// from guests at 10.43.0.1), a shelley.json is written pointing at the
// lease's LLM gateway, and the agent server is started on :9000
// (detached via setsid so the one-shot exec does not kill it). Returns
// JSON with the public web URL.
func runShelly(ctx context.Context, leaseID string) string {
	if _, err := resolveEndpoint(ctx, leaseID); err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	gw := *llmGatewayURL + leaseID
	script := fmt.Sprintf(`set -e
if [ ! -x /root/shelley ]; then
  curl -sf --max-time 180 %s -o /root/shelley
  chmod +x /root/shelley
fi
printf '{"llm_gateway":%q,"default_model":%q}' > /root/shelley.json
if [ -f /root/shelley.pid ]; then kill $(cat /root/shelley.pid) 2>/dev/null || true; rm -f /root/shelley.pid; fi
setsid /root/shelley --config /root/shelley.json serve --port 9000 --socket none >/root/shelley.log 2>&1 < /dev/null &
echo $! > /root/shelley.pid
sleep 6
if curl -sf --max-time 5 http://127.0.0.1:9000/version >/dev/null 2>&1; then
  echo SHELLEY_UP
else
  echo SHELLEY_DOWN; tail -5 /root/shelley.log
fi`, *shellyBinaryURL, gw, *shellyModel)
	payload, err := json.Marshal(map[string]any{"cmd": script, "timeout": 200})
	if err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+leaseID+"/exec", payload)
	if err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	var out struct {
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	_ = json.Unmarshal(b, &out)
	if !strings.Contains(out.Stdout, "SHELLEY_UP") {
		detail := strings.TrimSpace(out.Stdout)
		if out.Stderr != "" {
			detail += " | stderr: " + strings.TrimSpace(out.Stderr)
		}
		return fmt.Sprintf(`{"id":%q,"status":"failed","exit":%d,"detail":%q}`, leaseID, out.Exit, detail)
	}
	return fmt.Sprintf(`{"id":%q,"status":"started","url":"https://%s-9000.%s"}`, leaseID, leaseID, *gatewayHost)
}

func loadOrGenerateHostKey(path string) ssh.Signer {
	if k, err := loadKey(path); err == nil {
		return k
	}
	log.Printf("generating new host key %s", path)
	os.MkdirAll(filepath.Dir(path), 0o700)
	_, priv, err := ed25519Generate(path)
	if err != nil {
		log.Fatalf("host key: %v", err)
	}
	return priv
}

func loadOrGenerateKey(path string) ssh.Signer {
	if k, err := loadKey(path); err == nil {
		return k
	}
	log.Printf("generating new gateway identity %s", path)
	os.MkdirAll(filepath.Dir(path), 0o700)
	_, priv, err := ed25519Generate(path)
	if err != nil {
		log.Fatalf("gateway key: %v", err)
	}
	return priv
}

func loadKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}
