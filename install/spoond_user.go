// The spoond phase's item 12: the first admin user. When the identity
// store already holds a user it is kept; else --admin-key's public key
// is fingerprinted and POSTed with the consumer auth header and the
// bootstrap header, exactly the call of docs/install.md section 6. The
// consumer token rides the request headers (never the report): the
// fetcher interface carries them out of the phase's render.
package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/crypto/ssh"
)

// firstAdminUser creates the first user (docs/install.md section 6): a
// kept row when /var/lib/spoond/users.json holds at least one user, a
// created row after a 201 POST, a skipped row without --admin-key, a
// failed row on a POST the backend refused. The reporter sees names,
// never tokens.
func firstAdminUser(ctx context.Context, env *Env, r *Reporter, tk *tokens, dd hostDefaults) error {
	// users.json with a user: kept.
	if has, err := usersFileHasAny(env); err != nil {
		return err
	} else if has {
		r.Add(Kept, usersPath, "at least one user exists; the first admin user is kept")
		return nil
	}

	// No user and no key: a skipped row with the fix.
	if env.Opt.AdminKey == "" {
		r.Add(Skipped, "first admin user", "pass --admin-key ~/.ssh/id_ed25519.pub to create the first admin user")
		return nil
	}

	// Fingerprint the operator's public key.
	fp, err := sshFingerprintFile(env.Resolve(env.Opt.AdminKey))
	if err != nil {
		r.Add(Failed, env.Opt.AdminKey, "read the public key: "+err.Error())
		return err
	}

	if env.DryRun {
		r.Add(Created, "first admin user", "dry-run: POST "+backendURL+"/api/users would create admin from "+env.Opt.AdminKey)
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"name":         "admin",
		"kind":         "person",
		"fingerprints": []string{fp},
	})
	if err != nil {
		return fmt.Errorf("building the user POST: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL+"/api/users", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the user POST: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tk.consumer)
	req.Header.Set("X-Bootstrap-Token", tk.bootstrap)
	req.Header.Set("Content-Type", "application/json")

	fetch := env.healthFetcher(dd)
	code, err := fetch.Post(req)
	status := fmt.Sprintf("POST %s/api/users", backendURL)
	switch {
	case err != nil:
		note := status + " failed: " + err.Error()
		r.Add(Failed, "first admin user", note)
		return fmt.Errorf("%s", note)
	case code == http.StatusCreated:
		r.Add(Created, "first admin user", "admin created from "+env.Opt.AdminKey+" ("+fp+", the first user is the admin)")
		return nil
	case code == http.StatusForbidden:
		// Another user arrived between the read and the POST (or
		// BOOTSTRAP_TOKEN does not match the running backend): a
		// rerun answers kept.
		r.Add(Skipped, "first admin user", status+" answered 403; a user may already exist — rerun after checking "+usersPath)
		return nil
	default:
		note := fmt.Sprintf("%s answered %d; the backend rejected the first user", status, code)
		r.Add(Failed, "first admin user", note)
		return fmt.Errorf("%s", note)
	}
}

// usersFileHasAny reports whether users.json parses as a JSON array
// with at least one user (identity.Store's on-disk shape is a bare
// array). A missing file is no users; an empty array likewise.
func usersFileHasAny(env *Env) (bool, error) {
	full := env.Resolve(usersPath)
	b, err := os.ReadFile(full)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", usersPath, err)
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, nil
	}
	var users []json.RawMessage
	if err := json.Unmarshal(trimmed, &users); err != nil {
		return false, fmt.Errorf("parse %s: %w (users.json is the identity store's array of users)", usersPath, err)
	}
	return len(users) > 0, nil
}

// sshFingerprintFile fingerprints an SSH public key file with
// ssh.FingerprintSHA256, the same value docs/install.md section 6's
// `ssh-keygen -lf` prints (SHA256:<base64>).
func sshFingerprintFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(normalizeKeyLine(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w (want an authorized_keys line)", path, err)
	}
	return ssh.FingerprintSHA256(pub), nil
}

// normalizeKeyLine keeps a public key's first line only: some editors
// leave a UTF-8 BOM or trailing comment lines an authorized_keys parse
// tolerates anyway, but a multi-line paste is one key worth reading.
func normalizeKeyLine(data []byte) []byte {
	line := data
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return bytes.TrimSpace(line)
}
