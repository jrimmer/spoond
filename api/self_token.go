// Lease-scoped self token (#156): an opt-in credential a guest may hold
// to release or keep alive ONLY its own lease. A create sets
// "self_token": true; spoond mints a token, keeps only its sha256 hex on
// the lease and writes it, with the lease id and the API URL, into the
// guest's /run/secrets tmpfs beside the caller's own secrets. The token
// stays the same for the lease's life, so a guest may read it once: a
// re-stage (resume, restart, crash recovery, a named-snapshot save)
// writes the same token again, from this process's memory or, after a
// backend restart, by keeping the guest's copy when it still matches
// the stored hash. A new token is minted only when neither is there,
// and for a fork or clone child. It is revoked when the lease ends.

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jrimmer/spoond/v4/substrate"
)

const (
	// selfTokenPrefix marks a bearer as a lease-scoped self token.
	selfTokenPrefix = "spl_"

	// selfTokenBytes is the random material behind one token (32 bytes,
	// 256 bits).
	selfTokenBytes = 32

	// The guest file names reserved for the self token (and its lease
	// id and API URL). A create or exec secret may not use them.
	selfTokenSecretName = "spoond-lease-token"
	selfTokenIDSecret   = "spoond-lease-id"
	selfTokenURLSecret  = "spoond-api-url"
)

// selfTokenSecretNames is the reserved secret name set.
var selfTokenSecretNames = map[string]bool{
	selfTokenSecretName: true,
	selfTokenIDSecret:   true,
	selfTokenURLSecret:  true,
}

// reservedSecretName reports whether name is one of the self token's
// reserved guest file names.
func reservedSecretName(name string) bool { return selfTokenSecretNames[name] }

// newSelfToken mints one token: "spl_" plus unpadded base64url of 32
// random bytes. It returns the token and its sha256 hex, the only form
// ever stored.
func newSelfToken() (token, hash string, err error) {
	buf := make([]byte, selfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = selfTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, hashSelfToken(token), nil
}

// hashSelfToken is the stored form of a token.
func hashSelfToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// isSelfTokenBearer reports whether a bearer token is a lease-scoped
// self token (by its prefix).
func isSelfTokenBearer(token string) bool {
	return len(token) > len(selfTokenPrefix) && token[:len(selfTokenPrefix)] == selfTokenPrefix
}

// leaseBySelfToken finds the live lease whose current self token hash
// matches the token, comparing in constant time. It returns nil for an
// unknown, revoked or released lease's token.
func (s *Service) leaseBySelfToken(token string) *Lease {
	hash := hashSelfToken(token)
	var found *Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.SelfToken || l.SelfTokenHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(l.SelfTokenHash), []byte(hash)) == 1 {
			found = l
			break
		}
	}
	s.store.mu.Unlock()
	return found
}

// selfTokenFiles returns the guest files that carry a self-token
// lease's token, its lease id and the API URL (nil for any other
// lease). At create (restage false) the lease is not registered yet:
// it mints the token and sets the hash its first row saves. On a
// re-stage it keeps the current token — the one this process holds, or
// after a backend restart the guest's own copy when it matches the
// stored hash — and mints (and saves) a new one only when neither is
// there, so a guest that read its token once keeps a working one.
func (s *Service) selfTokenFiles(ctx context.Context, l *Lease, restage bool) (map[string]string, error) {
	s.store.mu.Lock()
	if !l.SelfToken || l.released {
		s.store.mu.Unlock()
		return nil, nil
	}
	hash := l.SelfTokenHash
	s.store.mu.Unlock()
	token := ""
	if restage && hash != "" {
		token = s.selfTokenFor(l.ID)
		if token == "" || subtle.ConstantTimeCompare([]byte(hashSelfToken(token)), []byte(hash)) != 1 {
			token = ""
			if b, err := s.sub.ReadFile(ctx, l.SandboxID, secretPath(selfTokenSecretName), selfTokenMaxBytes); err == nil &&
				subtle.ConstantTimeCompare([]byte(hashSelfToken(string(b))), []byte(hash)) == 1 {
				token = string(b)
			}
		}
	}
	if token == "" {
		var err error
		token, hash, err = newSelfToken()
		if err != nil {
			return nil, err
		}
		s.store.mu.Lock()
		if l.released {
			s.store.mu.Unlock()
			return nil, nil
		}
		l.SelfTokenHash = hash
		if restage {
			s.saveLeaseLocked(l)
		}
		s.store.mu.Unlock()
	}
	s.setSelfToken(l.ID, token)
	files := map[string]string{
		selfTokenSecretName: token,
		selfTokenIDSecret:   l.ID,
	}
	if s.cfg.SelfTokenAPIURL != "" {
		files[selfTokenURLSecret] = s.cfg.SelfTokenAPIURL
	}
	return files, nil
}

// selfTokenMaxBytes bounds the guest copy read back on a re-stage: a
// token is 47 bytes, so anything longer is not one.
const selfTokenMaxBytes = 128

// setSelfToken records a lease's current token. Memory only, beside the
// create-time secrets: the store keeps just the hash.
func (s *Service) setSelfToken(leaseID, token string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if s.selfTokens == nil {
		s.selfTokens = map[string]string{}
	}
	s.selfTokens[leaseID] = token
}

// selfTokenFor returns the token this process holds for a lease ("" when
// it has none, e.g. after a backend restart).
func (s *Service) selfTokenFor(leaseID string) string {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	return s.selfTokens[leaseID]
}

// stageCreateSecrets writes a lease's create-time secrets and, for a
// self-token lease, its self token with the lease id and the configured
// API URL (selfTokenFiles). restage is false at create, clone and fork,
// where the lease has no row yet. A lease with neither secrets nor a
// token writes nothing.
func (s *Service) stageCreateSecrets(ctx context.Context, l *Lease, secrets map[string]string, restage bool) error {
	files := make(map[string]string, len(secrets)+3)
	for name, value := range secrets {
		files[name] = value
	}
	extra, err := s.selfTokenFiles(ctx, l, restage)
	if err != nil {
		return fmt.Errorf("mint self token: %w", err)
	}
	for name, value := range extra {
		files[name] = value
	}
	if len(files) == 0 {
		return nil
	}
	return s.stageSecrets(ctx, l.SandboxID, files)
}

// leaseTokenID returns the lease id a request authenticated with a
// lease-scoped self token is bound to ("" and false for any other
// request).
func leaseTokenID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxLeaseToken{}).(string)
	return id, ok
}

// selfTokenPathID returns the lease id a request names when its shape is
// one a lease-scoped self token may reach at all: exactly
// DELETE /api/leases/{id} and POST /api/leases/{id}/keepalive. It runs
// in the auth middleware, before the mux has matched the request, so it
// reads the path itself and never trusts PathValue.
func selfTokenPathID(r *http.Request) (string, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/leases/")
	if !ok || rest == "" {
		return "", false
	}
	switch r.Method {
	case http.MethodDelete:
		if rest == "" || strings.Contains(rest, "/") {
			return "", false
		}
		return rest, true
	case http.MethodPost:
		id, ok := strings.CutSuffix(rest, "/keepalive")
		if !ok || id == "" || strings.Contains(id, "/") {
			return "", false
		}
		return id, true
	}
	return "", false
}

// selfTokenAllowance is the one host destination a self-token lease is
// let through to on top of its network policy (#156): the host's
// guest-facing address (HOST_GUEST_SERVICE_ADDR) on the port of
// SELF_TOKEN_API_URL, and nothing else on the host. Without it a
// restricted guest could hold its token but never present it. A lease
// without a self token, a "none" lease, or a host without both settings
// gets nothing. The URL's host name must resolve, from the guest, to
// that address. The caller holds s.store.mu.
func (s *Service) selfTokenAllowance(l *Lease) []substrate.PrivateAllowance {
	if !l.SelfToken || s.cfg.HostGuestAddr == "" {
		return nil
	}
	port := selfTokenAPIPort(s.cfg.SelfTokenAPIURL)
	if port == 0 {
		return nil
	}
	return []substrate.PrivateAllowance{{
		CIDR:     s.cfg.HostGuestAddr + "/32",
		TCPPorts: []uint32{port},
	}}
}

// selfTokenAPIPort is the TCP port a self-token API URL names: its
// explicit port, else 443 for https and 80 for http. 0 for an empty or
// unparseable URL.
func selfTokenAPIPort(raw string) uint32 {
	if raw == "" {
		return 0
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return 0
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return 0
		}
		return uint32(n)
	}
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}

// withAllowances appends each extra allowance not already in list.
func withAllowances(list []substrate.PrivateAllowance, extra []substrate.PrivateAllowance) []substrate.PrivateAllowance {
	for _, a := range extra {
		dup := false
		for _, b := range list {
			if b.CIDR == a.CIDR && slices.Equal(b.TCPPorts, a.TCPPorts) {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, a)
		}
	}
	return list
}
