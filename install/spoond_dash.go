// The spoond phase's items 6 and 7: the derived env files (admin-token,
// drain.env, the gateway env) and the dashboard's password and dash.env.
// Everything here is write-once (writeFileKeep): an existing file is
// kept, and its contents are never compared or printed.
package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

// writeDrainFiles writes /etc/spoond/admin-token, /etc/e2b/drain.env
// and /etc/spoond-gateway.env from the token set (item 6). An existing
// backend.env's tokens arrived in tk; these files take the same values
// (all files agree).
func writeDrainFiles(env *Env, r *Reporter, dd hostDefaults, tk *tokens) error {
	// admin-token: the ADMIN_TOKEN, no newline (the drain hook trims
	// trailing newlines anyway, but the file stays exact).
	if err := env.writeFileKeep(r, adminTokenPath, []byte(tk.admin)); err != nil {
		return err
	}

	// drain.env: what the orchestrator's hooks and the spoond-drain
	// unit read (docs/install.md section 6). SPOOND_DRAIN_INSECURE=1:
	// the call goes to 127.0.0.1 while the certificate is for the
	// host name.
	drain := map[string]string{
		"SPOOND_DRAIN_URL":        "https://127.0.0.1:8890",
		"SPOOND_ADMIN_TOKEN_FILE": adminTokenPath,
		"SPOOND_DRAIN_INSECURE":   "1",
	}
	if err := env.writeFileKeep(r, drainEnvPath, renderEnvFile(drain)); err != nil {
		return err
	}

	// /etc/spoond-gateway.env: what the shipped gateway unit reads
	// (deploy/spoond-gateway.env.example: the token, the public host,
	// the host-service address and port; the shelley and LLM URLs
	// derive from those two in the gateway itself).
	gateway := map[string]string{
		"SPOOND_GATEWAY_TOKEN":    tk.gateway,
		"SPOOND_GATEWAY_HOST":     dd.hostName,
		"HOST_GUEST_SERVICE_ADDR": dd.hostIP,
		"HOST_GUEST_SERVICE_PORT": "8891",
	}
	if err := env.writeFileKeep(r, gatewayEnvPath, renderEnvFile(gateway)); err != nil {
		return err
	}
	return nil
}

// dashPasswordLen is the generated dashboard password's character count:
// 20 lowercase hex characters, crypto/rand (10 bytes).
const dashPasswordLen = 10

// writeDashFiles writes /etc/spoond/dash-password (the generated
// password, 0600) and /etc/spoond/dash.env (the login and tokens the
// shipped dash unit reads, item 7). The row's note only says where the
// password is; the password and its hash reach no report.
//
// An existing dash-password is the source of truth: the keep rule keeps
// it and dash.env's hash keeps with it (the pair was written together;
// regenerating a password the dashboard then rejects is the failure to
// avoid). The bcrypt hash is made with the same call and cost `spoond
// dash hash` uses, so the printed hash and this one verify the same way.
//
// A fresh dash.env takes backend.env's METRICS_TOKEN; a kept dash.env
// keeps its own.
func writeDashFiles(env *Env, r *Reporter, tk *tokens, pair tlsPair) error {
	// A backend.env without METRICS_TOKEN leaves the token set empty:
	// mint one so the fresh dash.env never carries an empty token.
	if _, err := os.Stat(env.Resolve(dashEnvPath)); errors.Is(err, os.ErrNotExist) {
		if tk.metrics == "" {
			metrics, err := randHex(metricsTokenLen)
			if err != nil {
				return err
			}
			tk.metrics = metrics
		}
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", dashEnvPath, err)
	}
	full := env.Resolve(dashPasswordPath)
	pw, err := os.ReadFile(full)
	switch {
	case err == nil:
		r.Add(Kept, dashPasswordPath, "the dashboard password is in "+dashPasswordPath)
	case errors.Is(err, os.ErrNotExist):
		if env.DryRun {
			r.Add(Created, dashPasswordPath, "dry-run: a 20-character password would be written ("+dashPasswordPath+")")
			break
		}
		b := make([]byte, dashPasswordLen)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating the dashboard password: %w", err)
		}
		enc := hex.EncodeToString(b) // lowercase hex: 2×10 = 20 chars
		if err := env.writeFileKeep(r, dashPasswordPath, []byte(enc)); err != nil {
			return err
		}
		pw = []byte(enc)
	default:
		return fmt.Errorf("read %s: %w", dashPasswordPath, err)
	}

	if pw == nil {
		// Dry run on a fresh host: no password was generated, so no
		// hash is rendered either — the row already said what would
		// be written.
		r.Add(Created, dashEnvPath, "dry-run: the dashboard's login and tokens would be written")
		return nil
	}
	hash, hashErr := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
	if hashErr != nil {
		return fmt.Errorf("hashing the dashboard password: %w", hashErr)
	}

	// dash.env: DASH_USER, DASH_PASSWORD_HASH, METRICS_TOKEN,
	// DASH_EVENTS_TOKEN and the TLS pair — the exact spellings
	// cmd/spoond-dash reads (its package comment).
	dash := map[string]string{
		"DASH_USER":          "admin",
		"DASH_PASSWORD_HASH": string(hash),
		"METRICS_TOKEN":      tk.metrics,
		"DASH_EVENTS_TOKEN":  tk.events,
		"DASH_TLS_CERT":      pair.cert,
		"DASH_TLS_KEY":       pair.key,
	}
	return env.writeFileKeep(r, dashEnvPath, renderEnvFile(dash))
}
