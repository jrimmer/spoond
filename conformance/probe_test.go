//go:build conformance

package conformance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestProbeCmdRenderedCleanly pins the probe command construction: every
// template receives exactly its own arguments, so the rendered command
// carries no fmt error markers (`%!(EXTRA ...)` — whose parentheses break
// the guest shell), and both the host and the port are interpolated.
func TestProbeCmdRenderedCleanly(t *testing.T) {
	begin(t)
	const host = "10.11.0.4"
	for _, tc := range []struct {
		name string
		port int
	}{
		{"raw probe, port 9042", 9042},
		{"TLS probe, port 443", 443},
	} {
		cmd := probeCmd(host, tc.port)
		if strings.Contains(cmd, "%!") {
			failf(t, "%s: fmt error marker in rendered command: %q", tc.name, cmd)
		}
		if !strings.Contains(cmd, fmt.Sprintf("%q, %d", host, tc.port)) {
			failf(t, "%s: host/port not interpolated as (\"host\", port): %q", tc.name, cmd)
		}
		if tc.port == 443 && !strings.Contains(cmd, fmt.Sprintf("server_hostname=%q", host)) {
			failf(t, "%s: server_hostname not interpolated: %q", tc.name, cmd)
		}
	}
}

// TestProbe443VerdictsLocal runs the rendered port-443 probe against
// local throwaway listeners and checks every verdict path: handshake with
// silent-open -> ok, handshake with data -> ok, incoming TLS alert -> ok
// (a peer response proves the egress path is open), clean close before
// any peer bytes -> blocked, refused -> blocked. The certificate is
// self-signed and untrusted on purpose; the verdict is reachability, not
// trust.
func TestProbe443VerdictsLocal(t *testing.T) {
	begin(t)
	cert := selfSignedCert(t)
	run := func(port int) string {
		out, err := exec.Command("sh", "-c", probeCmd("127.0.0.1", port)).CombinedOutput()
		if err != nil {
			failf(t, "probe cmd failed: %v: %s", err, out)
		}
		token := strings.TrimSpace(string(out))
		if token != "ok" && token != "blocked" {
			failf(t, "probe output %q, want ok|blocked", token)
		}
		return token
	}

	// 1. Real TLS server, silent after handshake.
	ln := tlsListener(t, cert)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = s.Handshake()
		time.Sleep(3 * time.Second)
		s.Close()
	}()
	if got := run(ln.Addr().(*net.TCPAddr).Port); got != "ok" {
		failf(t, "handshake + silent-open: got %q, want ok", got)
	}
	ln.Close()

	// 2. Real TLS server that sends a byte after the handshake.
	ln = tlsListener(t, cert)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		if s.Handshake() == nil {
			_, _ = s.Write([]byte("X"))
		}
		s.Close()
	}()
	if got := run(ln.Addr().(*net.TCPAddr).Port); got != "ok" {
		failf(t, "handshake + data: got %q, want ok", got)
	}
	ln.Close()

	// 3. SNI-strict-server stand-in: read part of the ClientHello, answer
	// with a TLS alert record, then half-close so the client sees the
	// alert followed by FIN (no RST race).
	ln, alertDone := plainListener(t, func(c net.Conn) {
		buf := make([]byte, 4096)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Read(buf)                                     // the ClientHello
		_, _ = c.Write([]byte("\x15\x03\x03\x00\x02\x02\x50")) // alert: fatal internal_error
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		time.Sleep(300 * time.Millisecond)
		c.Close()
	})
	if got := run(ln.Addr().(*net.TCPAddr).Port); got != "ok" {
		failf(t, "incoming TLS alert: got %q, want ok (a peer response proves the path open)", got)
	}
	<-alertDone
	ln.Close()

	// 4. Denied-443 stand-in: read the ClientHello, close without any
	// peer bytes.
	ln, closeDone := plainListener(t, func(c net.Conn) {
		_, _ = io.CopyN(io.Discard, c, 1)
		c.Close()
	})
	if got := run(ln.Addr().(*net.TCPAddr).Port); got != "blocked" {
		failf(t, "clean close before any peer bytes: got %q, want blocked", got)
	}
	<-closeDone
	ln.Close()

	// 5. Refused: nothing listening.
	ln, _ = plainListener(t, func(c net.Conn) { c.Close() })
	refusedPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if got := run(refusedPort); got != "blocked" {
		failf(t, "refused connect: got %q, want blocked", got)
	}
}

// selfSignedCert mints a throw-alone TLS certificate for the local
// verdict test.
func selfSignedCert(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		failf(t, "tls test key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "probe-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		failf(t, "tls test cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		failf(t, "tls test key marshal: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		failf(t, "tls test key pair: %v", err)
	}
	return pair
}

func tlsListener(t *testing.T, cert tls.Certificate) net.Listener {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		failf(t, "tls listen: %v", err)
	}
	return ln
}

// plainListener serves each connection with fn in its own goroutine and
// returns a channel closed when the last served connection finished.
func plainListener(t *testing.T, fn func(net.Conn)) (net.Listener, <-chan struct{}) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		failf(t, "listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(done)
			return
		}
		fn(c)
		close(done)
	}()
	return ln, done
}
