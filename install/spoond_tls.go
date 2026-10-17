// The spoond phase's item 4: the TLS pair. With --tls-cert/--tls-key
// the existing files are pointed at (they must exist); without them a
// self-signed ECDSA P-256 pair is made in Go — CN and DNS SAN of the
// host name, the IP SAN of the host IP, ten years — and kept through
// writeFileKeep once written (a renewed pair is the operator's job).
package install

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// fileExists answers whether path stats OK.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// selfSignedYears is how long the self-signed pair is valid: ten
// years (docs/install.md section 4's -days 3650 shape).
const selfSignedYears = 10

// tlsPair is the answer the env files render: the certificate and key
// paths (/etc/spoond/tls/cert.pem and privkey.pem, or the operator's
// --tls-cert/--tls-key).
type tlsPair struct {
	cert, key string
}

// spoondTLS resolves the TLS pair (item 4). With --tls-cert/--tls-key:
// the paths must exist (the backend would start with a missing
// certificate only to fail later); without: a self-signed pair is made
// and kept. The rows name the files, never the key material.
func spoondTLS(env *Env, r *Reporter, dd hostDefaults) (tlsPair, error) {
	if env.Opt.TLSCert != "" {
		// Options.Validate already refuses one flag without the
		// other; both here must exist on the host.
		certFull, keyFull := env.Resolve(env.Opt.TLSCert), env.Resolve(env.Opt.TLSKey)
		for _, f := range []string{env.Opt.TLSCert, env.Opt.TLSKey} {
			if _, err := os.Stat(env.Resolve(f)); err != nil {
				note := fmt.Sprintf("%s is missing: point --tls-cert/--tls-key at an existing pair, or drop both flags for a self-signed one", f)
				r.Add(Failed, f, note)
				return tlsPair{}, fmt.Errorf("%s: %w", f, os.ErrNotExist)
			}
		}
		r.Add(Kept, env.Opt.TLSCert+" + "+env.Opt.TLSKey, "the operator's pair; backend.env and dash.env point at it")
		return tlsPair{cert: certFull, key: keyFull}, nil
	}

	// Self-signed. An existing pair is kept (it may be a Let's
	// Encrypt renewal the operator placed there); either half alone
	// would leave the backend a broken pair, so both orders fail
	// with the fix instead of silently keeping one.
	certFull, keyFull := env.Resolve(tlsCertPath), env.Resolve(tlsKeyPath)
	certOK, keyOK := fileExists(certFull), fileExists(keyFull)
	switch {
	case certOK && keyOK:
		r.Add(Kept, tlsCertPath, "the existing pair is kept ("+tlsKeyPath+" with it)")
		return tlsPair{cert: certFull, key: keyFull}, nil
	case certOK:
		r.Add(Failed, tlsCertPath, "exists but "+tlsKeyPath+" does not; remove the certificate (or install both) and rerun")
		return tlsPair{}, fmt.Errorf("%s exists but %s does not; remove the certificate (or install both) and rerun", tlsCertPath, tlsKeyPath)
	case keyOK:
		r.Add(Failed, tlsKeyPath, "exists but "+tlsCertPath+" does not; remove the key (or install both) and rerun")
		return tlsPair{}, fmt.Errorf("%s exists but %s does not; remove the key (or install both) and rerun", tlsKeyPath, tlsCertPath)
	}

	if env.DryRun {
		r.Add(Created, tlsCertPath, "dry-run: a self-signed pair for "+dd.hostName+" would be written")
		r.Add(Created, tlsKeyPath, "dry-run: nothing written")
		return tlsPair{cert: certFull, key: keyFull}, nil
	}
	if err := makeSelfSigned(env, r, dd); err != nil {
		return tlsPair{}, err
	}
	r.Add(Kept, "tls pair", "self-signed for "+dd.hostName+" (10 years), cert 0644, key 0600")
	return tlsPair{cert: certFull, key: keyFull}, nil
}

// makeSelfSigned generates the ECDSA P-256 pair and writes cert.pem
// (0644) and privkey.pem (0600) through the file helpers; the rows go
// to the phase's reporter.
func makeSelfSigned(env *Env, r *Reporter, dd hostDefaults) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating the TLS key: %w", err)
	}
	notBefore := time.Now().Add(-time.Hour) // clock skew
	notAfter := notBefore.Add(selfSignedYears*365*24*time.Hour + 24*time.Hour)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generating the serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dd.hostName},
		DNSNames:              []string{dd.hostName},
		IPAddresses:           []net.IP{net.ParseIP(dd.hostIP)},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating the self-signed certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshalling the TLS key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// The certificate is public (0644 via writeFile); the key is a
	// secret (0600 via writeFileKeep).
	if err := env.writeFile(r, tlsCertPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := env.writeFileKeep(r, tlsKeyPath, keyPEM); err != nil {
		return err
	}
	return nil
}
