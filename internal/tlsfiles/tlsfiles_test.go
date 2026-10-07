package tlsfiles

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair writes a self-signed certificate for names and its key under
// dir, returning the pair and the certificate's serial.
func writePair(t *testing.T, dir, base string, serial int64, names ...string) Pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := Pair{Cert: filepath.Join(dir, base+".crt"), Key: filepath.Join(dir, base+".key")}
	if err := os.WriteFile(p.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func serial(t *testing.T, s *Store, sni string) int64 {
	t.Helper()
	c, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf.SerialNumber.Int64()
}

func TestParse(t *testing.T) {
	if p, err := Parse("", ""); p != nil || err != nil {
		t.Fatalf("empty = %v, %v; want TLS off", p, err)
	}
	p, err := Parse("/a.crt, /b.crt", "/a.key,/b.key")
	if err != nil || len(p) != 2 || p[1] != (Pair{"/b.crt", "/b.key"}) {
		t.Fatalf("two pairs = %v, %v", p, err)
	}
	if _, err := Parse("/a.crt,/b.crt", "/a.key"); err == nil {
		t.Fatal("unequal lists must fail")
	}
	if _, err := Parse("/a.crt", ""); err == nil {
		t.Fatal("a certificate without a key must fail")
	}
}

func TestSNIPicksThePairAndDefaultsToTheFirst(t *testing.T) {
	dir := t.TempDir()
	s, err := New([]Pair{
		writePair(t, dir, "sb", 1, "sb.example.com"),
		writePair(t, dir, "alias", 2, "sandbox.example.com", "*.sandbox.example.com"),
		writePair(t, dir, "old", 3, "vm2.example.com"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for sni, want := range map[string]int64{
		"sb.example.com":          1,
		"SANDBOX.example.com.":    2,
		"x.sandbox.example.com":   2,
		"vm2.example.com":         3,
		"":                        1, // by IP: the first pair
		"other.example.com":       1,
		"a.b.sandbox.example.com": 1, // a wildcard covers one label only
	} {
		if got := serial(t, s, sni); got != want {
			t.Errorf("SNI %q got serial %d, want %d", sni, got, want)
		}
	}
}

func TestReloadPicksUpARenewal(t *testing.T) {
	dir := t.TempDir()
	p := writePair(t, dir, "sb", 1, "sb.example.com")
	s, err := New([]Pair{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reload(); n != 0 || err != nil {
		t.Fatalf("unchanged files reloaded %d, %v", n, err)
	}
	time.Sleep(10 * time.Millisecond) // a distinct mtime
	writePair(t, dir, "sb", 2, "sb.example.com")
	if n, err := s.Reload(); n != 1 || err != nil {
		t.Fatalf("renewal reloaded %d, %v", n, err)
	}
	if got := serial(t, s, "sb.example.com"); got != 2 {
		t.Fatalf("after renewal serial %d, want 2", got)
	}
}

func TestReloadKeepsTheOldPairWhenTheNewOneIsBroken(t *testing.T) {
	dir := t.TempDir()
	p := writePair(t, dir, "sb", 1, "sb.example.com")
	s, err := New([]Pair{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A renewal caught halfway: the new certificate with the old key.
	time.Sleep(10 * time.Millisecond)
	other := writePair(t, t.TempDir(), "x", 2, "sb.example.com")
	crt, _ := os.ReadFile(other.Cert)
	if err := os.WriteFile(p.Cert, crt, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reload(); n != 0 || err == nil {
		t.Fatalf("mismatched pair reloaded %d, %v; want 0 and an error", n, err)
	}
	if got := serial(t, s, "sb.example.com"); got != 1 {
		t.Fatalf("broken renewal replaced the certificate (serial %d)", got)
	}
	// The key lands: the next reload takes the complete pair.
	key, _ := os.ReadFile(other.Key)
	if err := os.WriteFile(p.Key, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Reload(); n != 1 || err != nil {
		t.Fatalf("completed renewal reloaded %d, %v", n, err)
	}
	if got := serial(t, s, "sb.example.com"); got != 2 {
		t.Fatalf("completed renewal serial %d, want 2", got)
	}
}

func TestNewFailsOnAMissingPair(t *testing.T) {
	dir := t.TempDir()
	good := writePair(t, dir, "sb", 1, "sb.example.com")
	if _, err := New([]Pair{good, {Cert: filepath.Join(dir, "no.crt"), Key: filepath.Join(dir, "no.key")}}, nil); err == nil {
		t.Fatal("a missing pair must fail startup")
	}
}
