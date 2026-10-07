package spoonddoctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestPair(t *testing.T, dir, name string, validFor time.Duration) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validFor)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	c, k := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(c, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return c, k
}

// TestCheckTLSEveryPair: each listed pair gets its own result; a pair
// expiring within 14 days warns; unequal lists fail.
func TestCheckTLSEveryPair(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := writeTestPair(t, dir, "sb.example.com", 60*24*time.Hour)
	c2, k2 := writeTestPair(t, dir, "vm2.example.com", 3*24*time.Hour)
	t.Setenv("TLS_CERT", c1+","+c2)
	t.Setenv("TLS_KEY", k1+","+k2)
	res := checkTLS()
	if len(res) != 2 || res[0].status != "PASS" || res[1].status != "WARN" || !strings.Contains(res[1].detail, "expires soon") {
		t.Fatalf("results = %+v", res)
	}
	if firstTLSCert() != c1 {
		t.Fatalf("firstTLSCert = %q, want %q", firstTLSCert(), c1)
	}
	t.Setenv("TLS_KEY", k1)
	if res := checkTLS(); len(res) != 1 || res[0].status != "FAIL" {
		t.Fatalf("unequal lists = %+v", res)
	}
}
