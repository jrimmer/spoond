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
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

var (
	// gatewayHost is the public hostname advertised in MOTDs.
	gatewayHost = flag.String("gateway-host", "sandbox.lacy.casa", "public hostname advertised in MOTDs")
	listenAddr  = flag.String("listen", ":2222", "listen address")
	hostKeyPath = flag.String("host-key", "/etc/forkd-gateway/ssh_host_ed25519_key", "path to SSH host key (generated if missing)")
	backendURL  = flag.String("backend", "https://127.0.0.1:8890", "forkd-backend base URL")
	backendTok  = flag.String("backend-token", "", "forkd-backend consumer token (required)")
	clientKeys  = flag.String("client-keys", "", "comma-separated paths to authorized client public keys")
	// gatewayKeyPath is the identity the gateway uses to connect INTO
	// sandboxes. Its public half is baked into dev-base authorized_keys.
	gatewayKeyPath = flag.String("gateway-key", "/etc/forkd-gateway/gateway_ed25519", "gateway identity key for nested connections")
	// shellyBinaryURL is where the `shelly` ctl verb fetches the agent
	// binary from inside the sandbox (host-side asset server on the
	// plain-HTTP proxy listener; guests reach it via forkd-br0).
	shellyBinaryURL = flag.String("shelly-binary-url", "http://10.43.0.1:8891/assets/shelley", "URL the sandbox fetches the shelley binary from")
	// shellyModel is the default model id written into shelley.json. It
	// must be an id the LLM gateway's LLM_MODEL_MAP understands (the
	// exe.dev catalog id, not the upstream id).
	shellyModel = flag.String("shelly-model", "gpt-oss-20b-fireworks", "default model id for the shelley agent")
)

type endpoint struct {
	ForkdID   string `json:"forkd_id"`
	Netns     string `json:"netns"`
	GuestAddr string `json:"guest_addr"`
	Image     string `json:"image"`
}

func main() {
	flag.Parse()
	if *backendTok == "" {
		log.Fatal("--backend-token is required")
	}

	hostKey := loadOrGenerateHostKey(*hostKeyPath)
	gatewayKey := loadOrGenerateKey(*gatewayKeyPath)

	allowed, err := loadAuthorizedKeys(*clientKeys)
	if err != nil {
		log.Fatalf("client keys: %v", err)
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if len(allowed) == 0 {
				return nil, fmt.Errorf("no client keys configured")
			}
			// Accept any key from the allowed set (single-user homelab;
			// the lease id in the username is the real capability).
			for _, pk := range allowed {
				if string(pk.Marshal()) == string(key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("unknown client key")
		},
	}
	config.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}
	log.Printf("forkd-sshd-gateway listening on %s", *listenAddr)

	for {
		conn, err := ln.Accept()
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

	go ssh.DiscardRequests(reqs)

	// The control plane is a reserved username: `ssh ctl@... "cmd"`.
	// Exec requests become API calls (new/ls/rm/keepalive/cp) and the
	// response is JSON on stdout — no sandbox is dialed.
	if user == "ctl" {
		handleControlPlane(chans, gatewayKey)
		return
	}

	// Resolve the target: a 32-hex lease id attaches an existing sandbox;
	// new[-<image>] auto-creates a persistent one (SSH-as-API).
	leaseID := user
	motd := ""
	if !isLeaseID(user) {
		created, img, err := createSandbox(context.Background(), user)
		if err != nil {
			errMsg := "forkd: " + err.Error() + "\n"
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
		motd = fmt.Sprintf("forkd: created sandbox %s (%s) — tmux 'dev' attached. Detach: Ctrl-b d. Reconnect: ssh %s@%s\n",
			created, img, created, *gatewayHost)
		log.Printf("created sandbox %s (%s) for user %q", created, img, user)
	}

	client, err := dialSandbox(context.Background(), leaseID, gatewayKey)
	if err != nil {
		log.Printf("dial sandbox for %s: %v", leaseID, err)
		// Tell the client what happened with a session-level error.
		for nc := range chans {
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.UnknownChannelType, "only session channels supported")
				continue
			}
			ch, _, _ := nc.Accept()
			fmt.Fprintf(ch, "forkd: cannot reach sandbox %s: %v\n", leaseID, err)
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

// createSandbox parses an SSH username of the form new[-<image>] and
// creates a persistent sandbox in the backend. Returns the new lease id
// and the resolved image tag.
func createSandbox(ctx context.Context, user string) (string, string, error) {
	image := "dev-base"
	alias := ""
	if user != "new" {
		rest, ok := strings.CutPrefix(user, "new-")
		if !ok || rest == "" {
			return "", "", fmt.Errorf("unknown command %q — use a lease id or new[-<image>] (new, new-dev, new-go, new-py, new-elixir, new-llm)", user)
		}
		alias = rest
	}
	if alias != "" {
		tag, ok := imageAliases[alias]
		if !ok {
			return "", "", fmt.Errorf("unknown image %q — try dev, go, py, elixir, llm", alias)
		}
		image = tag
	}

	// Interactive SSH requires an image with sshd. Only dev-base has it
	// today; CI images (go/py/elixir/llm) are for workflows, not shells.
	// Reject before creating so we don't orphan a sandbox.
	if image != "dev-base" {
		return "", "", fmt.Errorf("image %q is a CI image with no sshd — interactive SSH requires dev-base (use new or new-dev)", image)
	}

	payload, err := json.Marshal(map[string]any{
		"image":      image,
		"persistent": true,
		"ttl":        3600,
	})
	if err != nil {
		return "", "", err
	}
	b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes", payload)
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
func handleControlPlane(chans <-chan ssh.NewChannel, gatewayKey ssh.Signer) {
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
		out := runControlCommand(context.Background(), msg.Command, gatewayKey)
		ch.Write([]byte(out + "\n"))
		return
	}
}

// runControlCommand executes one control-plane command and returns the
// JSON-ish response text written to the client.
func runControlCommand(ctx context.Context, cmd string, gatewayKey ssh.Signer) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return `{"error":"empty command — try new, ls, rm, keepalive, cp, help"}`
	}
	switch fields[0] {
	case "help", "--help", "-h":
		return "commands: new [dev|go|py|elixir|llm], ls, rm <id>, keepalive <id>, suspend <id>, resume <id>, cp <id> [tag], shelly <id>"
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
		return strings.TrimSpace(string(b))
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
	default:
		return fmt.Sprintf(`{"error":"unknown command %q — try new, ls, rm, keepalive, cp, shelly, help"}`, fields[0])
	}
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

	// Show the MOTD (e.g. "created sandbox …") on new auto-created
	// sandboxes, before the nested shell output begins.
	if motd != "" {
		ch.Write([]byte(motd))
	}

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

// backendJSON performs a JSON request against the backend with the
// consumer token and returns the response body (or an error on non-2xx).
// Transient connection errors (refused/reset/timeout — e.g. backend busy
// refilling the warm pool) are retried with backoff; the gateway should
// not drop an SSH session because one loopback request got refused.
func backendJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		b, err := backendJSONOnce(ctx, method, path, body)
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := backendClient()
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
	gw := "http://10.43.0.1:8891/llm/" + leaseID
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
	return fmt.Sprintf(`{"id":%q,"status":"started","url":"https://%s-9000.sandbox.lacy.casa"}`, leaseID, leaseID)
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

func ed25519Generate(path string) (ssh.PublicKey, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	block, _ := ssh.MarshalPrivateKey(priv, "")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, nil, err
	}
	os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)
	return signer.PublicKey(), signer, nil
}

func loadAuthorizedKeys(csv string) ([]ssh.PublicKey, error) {
	var out []ssh.PublicKey
	if csv == "" {
		return out, nil
	}
	for _, p := range strings.Split(csv, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		for len(data) > 0 {
			pk, _, _, rest, err := ssh.ParseAuthorizedKey(data)
			if err != nil {
				break
			}
			out = append(out, pk)
			data = rest
		}
	}
	return out, nil
}
