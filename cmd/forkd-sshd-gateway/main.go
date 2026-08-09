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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

var (
	listenAddr  = flag.String("listen", ":2222", "listen address")
	hostKeyPath = flag.String("host-key", "/etc/forkd-gateway/ssh_host_ed25519_key", "path to SSH host key (generated if missing)")
	backendURL  = flag.String("backend", "https://127.0.0.1:8890", "forkd-backend base URL")
	backendTok  = flag.String("backend-token", "", "forkd-backend consumer token (required)")
	clientKeys  = flag.String("client-keys", "", "comma-separated paths to authorized client public keys")
	// gatewayKeyPath is the identity the gateway uses to connect INTO
	// sandboxes. Its public half is baked into dev-base authorized_keys.
	gatewayKeyPath = flag.String("gateway-key", "/etc/forkd-gateway/gateway_ed25519", "gateway identity key for nested connections")
)

type endpoint struct {
	ForkdID   string `json:"forkd_id"`
	Netns     string `json:"netns"`
	GuestAddr string `json:"guest_addr"`
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
	leaseID := sconn.User()
	log.Printf("conn: user=%s addr=%s", leaseID, sconn.RemoteAddr())

	go ssh.DiscardRequests(reqs)

	client, err := dialSandbox(context.Background(), leaseID, gatewayKey)
	if err != nil {
		log.Printf("dial sandbox for %s: %v", leaseID, err)
		// Tell the client what happened with a session-level error.
		return
	}
	defer client.Close()

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		go handleSession(newChan, client)
	}
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

func handleSession(newChan ssh.NewChannel, client *ssh.Client) {
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
	log.Printf("session relay starting")

	done := make(chan struct{}, 1)

	// Relay: client channel -> nested stdin, nested stdout/stderr ->
	// client channel.
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

	// Wait for the nested session to end or the client channel to close.
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
func backendJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
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
// pre-snapshot sshd listener does not.
func restartSSHD(ctx context.Context, leaseID string) error {
	cmd := "pkill -x sshd 2>/dev/null; sleep 1; mkdir -p /run/sshd; /usr/sbin/sshd 2>/dev/null; sleep 1; pgrep -x sshd >/dev/null || echo SSHD_NOT_RUNNING"
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
