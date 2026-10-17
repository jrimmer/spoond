// Small helpers of the spoond phase shared by its items: the
// 0700-directory rows (item 2) and the resolved TLS paths the env files
// point at.
package install

import (
	"errors"
	"fmt"
	"os"
)

// ensureSpoondDir creates one of the phase's directories at 0700 (item
// 2) and adds the row: kept when it already exists with the mode,
// created when the phase makes it (a dry run only plans). An existing
// directory with other modes is corrected like writeFile corrects a
// mode — the operator's umask must not leave a group-readable /etc/spoond.
func ensureSpoondDir(env *Env, r *Reporter, dir string) error {
	full := env.Resolve(dir)
	fi, err := os.Stat(full)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory; move it aside and rerun", dir)
		}
		if fi.Mode().Perm() != 0o700 {
			if env.DryRun {
				r.Add(Kept, dir, "dry-run: mode would be corrected to 0700")
				return nil
			}
			if err := os.Chmod(full, 0o700); err != nil {
				return fmt.Errorf("chmod %s: %w", full, err)
			}
			r.Add(Changed, dir, "mode was "+fmt.Sprintf("%#o", fi.Mode().Perm())+", corrected to 0700")
			return nil
		}
		r.Add(Kept, dir, "exists, 0700")
		return nil
	case errors.Is(err, os.ErrNotExist):
		if env.DryRun {
			r.Add(Created, dir, "dry-run: nothing created")
			return nil
		}
		if err := os.MkdirAll(full, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", full, err)
		}
		if err := os.Chmod(full, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", full, err)
		}
		r.Add(Created, dir, "created at 0700")
		return nil
	default:
		return fmt.Errorf("stat %s: %w", dir, err)
	}
}

// tlsCertFull is the certificate path the health fetcher verifies
// against: the operator's --tls-cert when given, else the phase's own
// /etc/spoond/tls/cert.pem (both resolve under Root).
func (env *Env) tlsCertFull() string {
	if env.Opt.TLSCert != "" {
		return env.Resolve(env.Opt.TLSCert)
	}
	return env.Resolve(tlsCertPath)
}
