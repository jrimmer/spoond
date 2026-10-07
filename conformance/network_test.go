//go:build conformance

package conformance

import (
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestN1_Policies checks the none, internet, lan and restricted egress
// policies.
func TestN1_Policies(t *testing.T) {
	begin(t)

	// none: nothing goes out.
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "none"})
	if canTCP(t, l.ID, "1.1.1.1", 443) {
		failf(t, "none: 1.1.1.1:443 reachable, want blocked")
	}
	if canTCP(t, l.ID, "10.0.0.203", 443) {
		failf(t, "none: 10.0.0.203:443 reachable, want blocked")
	}

	// internet: public and LAN reachable (internet maps to public plus the
	// LAN ranges).
	l = createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "internet"})
	if !canTCP(t, l.ID, "1.1.1.1", 443) {
		failf(t, "internet: 1.1.1.1:443 blocked, want reachable")
	}
	if !canTCP(t, l.ID, "10.0.0.203", 443) {
		failf(t, "internet: 10.0.0.203:443 blocked, want reachable")
	}

	// lan: private yes, public no; the host's own addresses are refused
	// except the granted service port (e2b only).
	l = createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "lan"})
	if !canTCP(t, l.ID, "10.0.0.203", 443) {
		failf(t, "lan: 10.0.0.203:443 blocked, want reachable")
	}
	if canTCP(t, l.ID, "1.1.1.1", 443) {
		failf(t, "lan: 1.1.1.1:443 reachable, want blocked")
	}
	if cfg.Substrate == "e2b" && canTCP(t, l.ID, "10.0.0.11", 22) {
		failf(t, "lan: host 10.0.0.11:22 reachable, want refused")
	}

	// restricted with an allowlist: example.com answers, google fails.
	l = createLease(t, map[string]any{
		"image":            "py-base",
		"ttl":              600,
		"network_policy":   "restricted",
		"egress_allowlist": []string{"example.com"},
	})
	code := execOK(t, l.ID, "curl -sS -o /dev/null -w '%{http_code}' https://example.com")
	if !threeDigits.MatchString(code) {
		failf(t, "restricted allowlist: example.com curl code = %q, want a 3-digit code", code)
	}
	st, body, err := cl.exec(l.ID, execReq{Cmd: "curl -sS -o /dev/null -w '%{http_code}' https://www.google.com"})
	if err != nil {
		failf(t, "restricted allowlist: google exec: %v", err)
	}
	if st != 200 {
		failf(t, "restricted allowlist: google exec: status %d: %s", st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "restricted allowlist: google exec: bad body: %v", err)
	}
	if out.Exit == 0 {
		failf(t, "restricted allowlist: https://www.google.com unexpectedly succeeded")
	}
}

// TestN9_MixedRestrictedAllowlist pins the mixed IP/domain allowlist: a
// restricted lease that names a private (LAN) address and a public domain
// must reach both, and must still block a public host and a private
// address it does not name. Regression test for spoond-4pa: before the
// fix, adding any domain to a restricted allowlist made the private IPs
// unreachable (the guest's allowed_cidrs carried the DNS fallback as a
// bare "8.8.8.8", which the fork's layer-2 decision rejected as an
// invalid CIDR for every connection, and a domain that resolved to an
// allow-listed LAN IP was refused on the SNI path).
//
// Environment variables (all read by harness_test.go):
//
//	CONFORMANCE_MIXED_PRIVATE          private IP the lease allowlists
//	                                   (no default; vm2 must set it, or
//	                                   the case skips)
//	CONFORMANCE_MIXED_PRIVATE_PORT     its TLS port, default 443
//	CONFORMANCE_MIXED_DOMAIN           public domain the lease allowlists,
//	                                   default example.com
//	CONFORMANCE_MIXED_BLOCKED_PRIVATE  private IP the lease does not
//	                                   allowlist, expected blocked (no
//	                                   default; skips when unset)
func TestN9_MixedRestrictedAllowlist(t *testing.T) {
	begin(t)

	// No LAN is assumed: without the vm2-provided private addresses the
	// reachable/blocked checks would be vacuous or spuriously failing.
	if cfg.MixedPrivate == "" || cfg.MixedBlockedPrivate == "" {
		skipf(t, "CONFORMANCE_MIXED_PRIVATE and CONFORMANCE_MIXED_BLOCKED_PRIVATE are unset; set them (vm2 does) to exercise the mixed allowlist")
	}

	l := createLease(t, map[string]any{
		"image":            "py-base",
		"ttl":              900,
		"network_policy":   "restricted",
		"egress_allowlist": []string{cfg.MixedPrivate, cfg.MixedDomain},
	})

	// The allow-listed private (LAN) address is reachable over its TLS
	// port, even though the same allowlist names a domain.
	if !canTCP(t, l.ID, cfg.MixedPrivate, cfg.MixedPrivatePort) {
		failf(t, "mixed allowlist: private %s:%d blocked, want reachable", cfg.MixedPrivate, cfg.MixedPrivatePort)
	}

	// The allow-listed public domain is reachable.
	if !canTCP(t, l.ID, cfg.MixedDomain, 443) {
		failf(t, "mixed allowlist: domain %s blocked, want reachable", cfg.MixedDomain)
	}

	// A public host that is not named is still blocked.
	if canTCP(t, l.ID, "www.google.com", 443) {
		failf(t, "mixed allowlist: unlisted public www.google.com reachable, want blocked")
	}

	// A private address that is not named is still blocked.
	if canTCP(t, l.ID, cfg.MixedBlockedPrivate, 443) {
		failf(t, "mixed allowlist: unlisted private %s reachable, want blocked", cfg.MixedBlockedPrivate)
	}
}

// TestN2_LivePolicyChange switches a lease from none to internet while it
// runs. Requires the U09 network route.
func TestN2_LivePolicyChange(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "none"})
	if canTCP(t, l.ID, "1.1.1.1", 443) {
		failf(t, "none: 1.1.1.1:443 reachable, want blocked")
	}

	st, body, err := cl.network(l.ID, map[string]any{"network_policy": "internet"})
	if err != nil {
		failf(t, "network: %v", err)
	}
	if st != 200 {
		failf(t, "network: status %d: %s", st, truncate(body))
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if canTCP(t, l.ID, "1.1.1.1", 443) {
			return
		}
		if time.Now().After(deadline) {
			failf(t, "policy change to internet not effective within 5s")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestN3_ProxyURL reaches a guest HTTP server through the public proxy
// listener with the forward-auth headers.
func TestN3_ProxyURL(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	execOK(t, l.ID, "nohup python3 -m http.server 8080 >/dev/null 2>&1 &")
	time.Sleep(1 * time.Second)

	st, body, err := cl.proxyGet(l.ID, 8080)
	if err != nil {
		failf(t, "proxy GET: %v", err)
	}
	if st != 200 {
		failf(t, "proxy GET: status %d: %s", st, truncate([]byte(body)))
	}
	if !strings.Contains(body, "Directory listing") {
		failf(t, "proxy GET: body lacks Directory listing: %s", truncate([]byte(body)))
	}
}

// TestN4_ExposePortsPeer checks that an exposed port of one lease is
// reachable from peers according to their policy, and that unexposed ports
// are not.
func TestN4_ExposePortsPeer(t *testing.T) {
	begin(t)
	scylla := createLease(t, map[string]any{"image": "scylla", "expose_ports": []int{9042}, "ttl": 900})
	addr, ok := scylla.Exposed["9042"]
	if !ok {
		failf(t, "scylla: exposed lacks 9042: %v", scylla.Exposed)
	}
	h, p, err := splitHostPort(addr)
	if err != nil {
		failf(t, "scylla: exposed[9042] = %q: %v", addr, err)
	}

	a := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "internet"})
	if !canTCP(t, a.ID, h, p) {
		failf(t, "internet peer: %s reachable? want yes", addr)
	}
	b := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "none"})
	if canTCP(t, b.ID, h, p) {
		failf(t, "none peer: %s reachable, want blocked", addr)
	}
	if canTCP(t, a.ID, h, 22) {
		failf(t, "internet peer: %s:22 reachable, want blocked (only exposed ports are)", h)
	}
}

// TestN5_SSHGateway exercises the SSH gateway: SSH-as-API create (user
// new), a PTY session, non-PTY exec, SFTP, and the ctl control plane.
func TestN5_SSHGateway(t *testing.T) {
	begin(t)

	signer := sshKey(t)
	sshCfg := &ssh.ClientConfig{
		User:            "new",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	before := ctlLeaseIDs(t, signer)

	// Step 1: connect as "new", which creates a persistent dev-base lease.
	client, err := ssh.Dial("tcp", cfg.SSHGateway, sshCfg)
	if err != nil {
		failf(t, "ssh new: %v", err)
	}
	defer client.Close()

	// Step 2: interactive session — echo SSHOK over a PTY.
	sess, err := client.NewSession()
	if err != nil {
		failf(t, "new session: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		failf(t, "stdout pipe: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		failf(t, "stdin pipe: %v", err)
	}
	if err := sess.RequestPty("xterm", 80, 24, ssh.TerminalModes{}); err != nil {
		failf(t, "request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		failf(t, "shell: %v", err)
	}
	var outBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				outBuf.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	if _, err := stdin.Write([]byte("echo SSHOK\n")); err != nil {
		failf(t, "write stdin: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(outBuf.String(), "SSHOK") {
		if time.Now().After(deadline) {
			failf(t, "no SSHOK within 15s; output so far: %q", truncate([]byte(outBuf.String())))
		}
		time.Sleep(100 * time.Millisecond)
	}
	sess.Close()

	// Step 3: non-PTY exec.
	out := sshExec(t, client, "uname -s")
	if strings.TrimSpace(out) != "Linux" {
		failf(t, "uname -s = %q, want Linux", out)
	}

	// Step 4: SFTP round-trip of 1 MiB.
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		failf(t, "sftp: %v", err)
	}
	defer sftpClient.Close()
	payload := make([]byte, 1<<20)
	if _, err := crand.Read(payload); err != nil {
		failf(t, "random payload: %v", err)
	}
	remote := "/tmp/conformance-sftp-" + randMarker()
	f, err := sftpClient.Create(remote)
	if err != nil {
		failf(t, "sftp create %s: %v", remote, err)
	}
	if _, err := f.Write(payload); err != nil {
		failf(t, "sftp write: %v", err)
	}
	f.Close()
	rf, err := sftpClient.Open(remote)
	if err != nil {
		failf(t, "sftp open: %v", err)
	}
	got, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		failf(t, "sftp read: %v", err)
	}
	if !bytes.Equal(payload, got) {
		failf(t, "sftp round-trip mismatch: sent %d bytes, got %d", len(payload), len(got))
	}

	// Step 5: the control plane lists the created lease (found the same
	// way for cleanup).
	after := ctlLeaseIDs(t, signer)
	var created []string
	for id := range after {
		if !before[id] {
			created = append(created, id)
		}
	}
	if len(created) != 1 {
		failf(t, "expected exactly one lease created by the gateway, got %v", created)
	}
	leaseID := created[0]
	track(t, leaseID)
	if !after[leaseID] {
		failf(t, "ctl ls --json does not contain the created lease %s", leaseID)
	}
}

// TestN6_LLMGateway checks that the host service address is reachable from
// a default restricted lease.
func TestN6_LLMGateway(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "network_policy": "restricted"})
	if !canTCPAddr(t, l.ID, cfg.GuestService) {
		failf(t, "restricted: %s blocked, want reachable (granted service port)", cfg.GuestService)
	}
}

// --- SSH helpers ---

func sshKey(t *testing.T) ssh.Signer {
	pem, err := os.ReadFile(cfg.SSHKey)
	if err != nil {
		failf(t, "read SSH key: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		failf(t, "parse SSH key: %v", err)
	}
	return signer
}

// sshExec runs one non-PTY exec over an established SSH connection.
func sshExec(t *testing.T, client *ssh.Client, cmd string) string {
	sess, err := client.NewSession()
	if err != nil {
		failf(t, "new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		if _, missing := err.(*ssh.ExitMissingError); !missing {
			failf(t, "exec %q: %v", cmd, err)
		}
	}
	return string(out)
}

// ctlLeaseIDs connects to the gateway as ctl and lists the caller's lease
// ids from `ls --json`.
func ctlLeaseIDs(t *testing.T, signer ssh.Signer) map[string]bool {
	cfgCtl := &ssh.ClientConfig{
		User:            "ctl",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	client, err := ssh.Dial("tcp", cfg.SSHGateway, cfgCtl)
	if err != nil {
		failf(t, "ssh ctl: %v", err)
	}
	defer client.Close()
	out := sshExec(t, client, "ls --json")
	var resp struct {
		Sandboxes []leaseInfo `json:"sandboxes"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		failf(t, "ctl ls --json: bad body %q: %v", truncate([]byte(out)), err)
	}
	ids := map[string]bool{}
	for _, l := range resp.Sandboxes {
		ids[l.ID] = true
	}
	return ids
}
