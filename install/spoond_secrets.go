// The spoond phase's items 3 and 5: the tokens, backend.env (the
// backend's main operator file), and the token-consistency answer the
// derived files need. Every byte of this file's renders goes through
// env.writeFileKeep — an existing file is never overwritten — and the
// rendered values never reach the report.
package install

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Secret sizes, bytes (docs/install.md section 4's openssl rand -hex N:
// 24 = 48 hex chars, 32 = 64 hex chars).
const (
	consumerTokenLen  = 24 // 48 hex chars
	bootstrapTokenLen = 24
	gatewayTokenLen   = 32 // 64 hex chars
	adminTokenLen     = 32
	metricsTokenLen   = 32
	eventsTokenLen    = 32
)

// tokens is one set of the six secrets the files must agree on. When
// backend.env already exists its five token keys fill these (and all
// the derived files take them); else they are generated here.
type tokens struct {
	consumer  string
	bootstrap string
	gateway   string
	admin     string
	metrics   string
	events    string
}

// firstConsumerToken reads the first token=consumer pair's token from a
// CONSUMER_TOKENS value: what the first-user POST's Authorization header
// carries (the docs' ${CONSUMER_TOKENS%%=*}; the backend resolves the
// owner through the bare token). Empty when the value holds no pairs.
func firstConsumerToken(consumerTokens string) string {
	first, _, _ := strings.Cut(consumerTokens, ",")
	s, _, ok := strings.Cut(first, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// randHex reads n bytes of crypto/rand and hex-encodes them lowercase:
// the generated secret's shape (openssl rand -hex N).
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// parseEnvFile reads KEY=VALUE lines, so an existing backend.env can
// drive the derived files (all files agree). Comments and blanks are
// skipped; a line without = is skipped rather than an error (systemd
// keeps no comments and the file is machine-written, but an operator's
// stray line must not abort the run).
func parseEnvFile(path string) (map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

// renderEnvFile renders KEY=VALUE lines in the map's key order (sorted,
// so renders are reproducible and files stay diffable).
func renderEnvFile(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(m[k])
		buf.WriteByte('\n')
	}
	return []byte(buf.String())
}

// backendEnvWrite reads the phase's token set (item 3): when
// backend.env already exists it parses (KEY=VALUE lines) and uses its
// GATEWAY_TOKEN, ADMIN_TOKEN, EVENTS_TOKEN, BOOTSTRAP_TOKEN and
// CONSUMER_TOKENS for the derived files below, so all files agree;
// the row says kept. Without it, the six secrets are generated
// (crypto/rand, lowercase hex) and the file written fresh (item 5's
// minimum contents, docs/install.md section 4), never overwriting
// anything. It answers the token set the derived files must carry and
// whether this run wrote (or would write) backend.env.
//
// The TLS_CERT/TLS_KEY paths rendered are the resolved pair's (the
// caller spoondTLS ran first: an operator --tls-cert failure stops the
// phase before an env file carries the pair).
func backendEnvWrite(env *Env, r *Reporter, dd hostDefaults, pair tlsPair) (*tokens, map[string]bool, error) {
	states := map[string]bool{}
	envPath := env.Resolve(backendEnvPath)
	if _, err := os.Stat(envPath); err == nil {
		// Kept. Parse for the derived files. Only the five token
		// keys matter; a missing one fails (the derived files
		// must carry the same tokens, docs/install.md section 4:
		// no mixed secrets).
		r.Add(Kept, backendEnvPath, "exists, left untouched")
		vals, err := parseEnvFile(envPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: parse %w", backendEnvPath, err)
		}
		var missing []string
		want := []string{"GATEWAY_TOKEN", "ADMIN_TOKEN", "EVENTS_TOKEN", "BOOTSTRAP_TOKEN", "CONSUMER_TOKENS"}
		for _, k := range want {
			if vals[k] == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return nil, nil, fmt.Errorf("%s: token variables %s not set; rerun after setting them (a fresh install generates them)", backendEnvPath, strings.Join(missing, ", "))
		}
		consumer := firstConsumerToken(vals["CONSUMER_TOKENS"])
		if consumer == "" {
			// CONSUMER_TOKENS is a bearer-token list: the failure names
			// the key, never its value.
			return nil, nil, fmt.Errorf("%s: CONSUMER_TOKENS does not parse as token=consumer pairs (token=consumer,key=consumer,...); repair it and rerun", backendEnvPath)
		}
		// METRICS_TOKEN is taken from backend.env when it has one, so
		// dash.env scrapes with the backend's own token; writeDashFiles
		// mints one only for a backend.env without it.
		tk := &tokens{
			consumer:  consumer,
			bootstrap: vals["BOOTSTRAP_TOKEN"],
			gateway:   vals["GATEWAY_TOKEN"],
			admin:     vals["ADMIN_TOKEN"],
			metrics:   vals["METRICS_TOKEN"],
			events:    vals["EVENTS_TOKEN"],
		}
		return tk, states, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("stat %s: %w", envPath, err)
	}

	// Fresh deployment: generate the six secrets.
	consumer, err := randHex(consumerTokenLen)
	if err != nil {
		return nil, nil, err
	}
	bootstrap, err := randHex(bootstrapTokenLen)
	if err != nil {
		return nil, nil, err
	}
	gateway, err := randHex(gatewayTokenLen)
	if err != nil {
		return nil, nil, err
	}
	admin, err := randHex(adminTokenLen)
	if err != nil {
		return nil, nil, err
	}
	metrics, err := randHex(metricsTokenLen)
	if err != nil {
		return nil, nil, err
	}
	events, err := randHex(eventsTokenLen)
	if err != nil {
		return nil, nil, err
	}

	m := map[string]string{
		"CONSUMER_TOKENS":           consumer + "=local",
		"SPOOND_DB_PATH":            "/var/lib/spoond/spoond.db",
		"E2B_GRPC_ADDR":             "127.0.0.1:5008",
		"E2B_PROXY_URL":             "http://127.0.0.1:5007",
		"E2B_TOKEN_SEED_FILE":       e2bTokenSeedPath,
		"IMAGE_REGISTRY":            "localhost:5000",
		"HOST_GUEST_SERVICE_ADDR":   dd.hostIP,
		"HOST_GUEST_SERVICE_PORT":   "8891",
		"SPOOND_GUEST_DNS_ADDR":     dd.guestDNS,
		"SPOOND_PROXY_HOST_SUFFIX":  "." + dd.hostName,
		"E2B_TEMPLATE_STORAGE_PATH": env.Opt.storeDir() + "/storage/templates",
		"SPOOND_BACKUP_DIR":         "/var/lib/spoond/backups",
		"USERS_FILE":                usersPath,
		"BOOTSTRAP_TOKEN":           bootstrap,
		"GATEWAY_TOKEN":             gateway,
		"ADMIN_TOKEN":               admin,
		"METRICS_TOKEN":             metrics,
		"EVENTS_TOKEN":              events,
		"TLS_CERT":                  pair.cert,
		"TLS_KEY":                   pair.key,
		"BIND_ADDR":                 "0.0.0.0:8890",
		"PROXY_ADDR":                "0.0.0.0:8891",
	}
	if err := env.writeFileKeep(r, backendEnvPath, renderEnvFile(m)); err != nil {
		return nil, nil, err
	}
	states[backendEnvPath] = true
	tk := &tokens{
		consumer:  consumer,
		bootstrap: bootstrap,
		gateway:   gateway,
		admin:     admin,
		metrics:   metrics,
		events:    events,
	}
	return tk, states, nil
}
