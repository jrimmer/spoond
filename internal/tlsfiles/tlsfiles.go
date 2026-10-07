// Package tlsfiles serves TLS certificates from files: one or more
// certificate/key pairs on one listener, chosen by the client's SNI, and
// reloaded when the files change so a renewal needs no restart.
//
// The backend (TLS_CERT, TLS_KEY) and the dashboard (DASH_TLS_CERT,
// DASH_TLS_KEY) take comma-separated lists of equal length, pairing the
// n-th certificate with the n-th key. The first pair is the default: it is
// served when the client sends no SNI (a connection by IP) or a name no
// certificate covers.
package tlsfiles

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Pair is one certificate chain file and its private key file.
type Pair struct {
	Cert, Key string
}

// Parse splits comma-separated certificate and key lists into pairs.
// Both empty means TLS is off (nil, nil); otherwise the lists must have
// the same, non-zero length.
func Parse(certs, keys string) ([]Pair, error) {
	if strings.TrimSpace(certs) == "" && strings.TrimSpace(keys) == "" {
		return nil, nil
	}
	cs, ks := split(certs), split(keys)
	if len(cs) == 0 || len(ks) == 0 {
		return nil, fmt.Errorf("set both the certificate and the key list, or neither")
	}
	if len(cs) != len(ks) {
		return nil, fmt.Errorf("%d certificate(s) but %d key(s): the lists pair up by position", len(cs), len(ks))
	}
	out := make([]Pair, len(cs))
	for i := range cs {
		out[i] = Pair{Cert: cs[i], Key: ks[i]}
	}
	return out, nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type loaded struct {
	cert  *tls.Certificate
	names []string // the leaf's DNS SANs (or its CN when it has none)
	stamp string   // the files' mtimes and sizes when loaded
}

// Store holds the loaded pairs. Its zero value is not usable; use New.
type Store struct {
	pairs []Pair
	logf  func(string, ...any)

	mu     sync.RWMutex
	loaded []loaded
}

// New loads every pair. It fails if any pair cannot be loaded: a server
// must not start with a certificate missing.
func New(pairs []Pair, logf func(string, ...any)) (*Store, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no certificate pairs")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Store{pairs: pairs, logf: logf, loaded: make([]loaded, len(pairs))}
	for i, p := range pairs {
		l, err := load(p)
		if err != nil {
			return nil, err
		}
		s.loaded[i] = l
	}
	return s, nil
}

func stamp(p Pair) (string, error) {
	var b strings.Builder
	for _, f := range []string{p.Cert, p.Key} {
		st, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d/%d;", st.ModTime().UnixNano(), st.Size())
	}
	return b.String(), nil
}

func load(p Pair) (loaded, error) {
	st, err := stamp(p)
	if err != nil {
		return loaded{}, fmt.Errorf("tls pair %s: %w", p.Cert, err)
	}
	c, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		return loaded{}, fmt.Errorf("tls pair %s + %s: %w", p.Cert, p.Key, err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return loaded{}, fmt.Errorf("tls pair %s: %w", p.Cert, err)
	}
	c.Leaf = leaf
	names := leaf.DNSNames
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}
	return loaded{cert: &c, names: names, stamp: st}, nil
}

// GetCertificate picks the pair whose names cover the client's SNI
// (exact, or a one-label wildcard), else the first pair.
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name != "" {
		for _, l := range s.loaded {
			for _, n := range l.names {
				if matches(strings.ToLower(n), name) {
					return l.cert, nil
				}
			}
		}
	}
	return s.loaded[0].cert, nil
}

func matches(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		if i := strings.IndexByte(name, '.'); i > 0 {
			return name[i+1:] == rest
		}
	}
	return false
}

// Reload re-reads every pair whose files changed. A pair that fails to
// load (a renewal caught between writing the certificate and the key, or a
// missing file) keeps serving its previous certificate; the error is
// returned and the next Reload tries again. It reports how many pairs
// were replaced.
func (s *Store) Reload() (int, error) {
	var errs []string
	n := 0
	for i, p := range s.pairs {
		st, err := stamp(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		s.mu.RLock()
		same := st == s.loaded[i].stamp
		s.mu.RUnlock()
		if same {
			continue
		}
		l, err := load(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		s.mu.Lock()
		s.loaded[i] = l
		s.mu.Unlock()
		n++
		s.logf("tls: reloaded %s (%s, expires %s)", p.Cert, strings.Join(l.names, ","), l.cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if len(errs) > 0 {
		return n, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return n, nil
}

// Watch calls Reload every interval until ctx ends, logging failures (the
// previous certificates keep serving).
func (s *Store) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Reload(); err != nil {
				s.logf("tls: reload kept the previous certificate(s): %v", err)
			}
		}
	}
}

// Config returns a server TLS config that serves from the store.
func (s *Store) Config() *tls.Config {
	return &tls.Config{GetCertificate: s.GetCertificate, MinVersion: tls.VersionTLS12}
}

// Names lists each pair's certificate names, for startup logs.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.loaded))
	for i, l := range s.loaded {
		out[i] = strings.Join(l.names, ",")
	}
	return out
}
