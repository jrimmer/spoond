// The spoond phase (docs/plans/2026-10-16-4.0-install.md, S5): spoond's
// own binary, secrets, env files, TLS pair, units and first admin user —
// today's manual steps of docs/install.md sections 4 and 6, idempotent
// so a rerun keeps every secret and reports each item it left alone.
//
// The items, in the order the phase reports them (the task item
// numbers):
//
//  1. defaults: hostname, host IP, guest DNS, the token seed (query)
//  2. directories /etc/spoond, /etc/spoond/tls, /var/lib/spoond(+/backups)
//  3. the secrets and backend.env, never overwritten once written
//  4. the TLS pair (--tls-cert/--tls-key or a self-signed P-256 pair)
//  5. backend.env (also item 3's file)
//  6. /etc/spoond/admin-token, /etc/e2b/drain.env, /etc/spoond-gateway.env
//  7. the dashboard password and dash.env
//  8. --container --config-only stops here
//  9. the binary copy to /opt/spoond/spoond (skipped under --container)
//
// 10. the systemd units, daemon-reload, enable --now, the backend restart
// 11. the backend's /healthz
// 12. the first admin user (--admin-key; else a skipped row)
// 13. the runner row (always skipped)
package install

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/deploy"
)

// Production paths and constants the spoond phase owns. Every file name
// is what the shipped units and docs/install.md read; changing one here
// and one in deploy/ or docs/install.md goes together.
const (
	// backendEnvPath is the backend's main operator file (0600,
	// EnvironmentFile=-/etc/spoond/backend.env in the shipped unit).
	backendEnvPath = "/etc/spoond/backend.env"
	// adminTokenPath holds the ADMIN_TOKEN for the drain hook (0600;
	// hooks read tokens from files, never the environment).
	adminTokenPath = "/etc/spoond/admin-token"
	// drainEnvPath is the orchestrator drain hook's configuration.
	drainEnvPath = "/etc/e2b/drain.env"
	// gatewayEnvPath is spoond-sshd-gateway's environment file (the
	// unit reads it: EnvironmentFile=/etc/spoond-gateway.env).
	gatewayEnvPath = "/etc/spoond-gateway.env"
	// dashEnvPath and dashPasswordPath are the dashboard's
	// configuration (0600): dash-password holds the generated
	// password, dash.env the bcrypt hash and the tokens.
	dashEnvPath      = "/etc/spoond/dash.env"
	dashPasswordPath = "/etc/spoond/dash-password"
	// tlsCertPath and tlsKeyPath are the backend and dashboard TLS
	// pair (docs/install.md section 4: /etc/spoond/tls/).
	tlsCertPath = "/etc/spoond/tls/cert.pem"
	tlsKeyPath  = "/etc/spoond/tls/privkey.pem"
	// e2bTokenSeedPath is the envd/traffic HMAC seed the runtime
	// phase generates; backend.env only points at it.
	e2bTokenSeedPath = "/etc/spoond/e2b-token-seed"
	// usersPath is the identity store (docs/install.md section 6).
	usersPath = "/var/lib/spoond/users.json"

	// spoondBinPath is the installed binary the units exec.
	spoondBinPath = "/opt/spoond/spoond"
	// orchestratorUnitPath is the shipped e2b orchestrator unit, with
	// its drain hooks: written over the drain-less copy the runtime
	// phase installed (docs/install.md sections 2 and 4).
	orchestratorUnitPath = "/etc/systemd/system/e2b-orchestrator.service"

	// backendPort is the lease API's port: the health wait and the
	// bootstrap POST go to https://127.0.0.1:<backendPort>/.
	backendPort = "8890"
	// backendURL is the backend the health wait and the bootstrap
	// POST call: the lease API's own listener on the loopback.
	backendURL = "https://127.0.0.1:" + backendPort
	// healthWait is how long the health item waits for the backend's
	// /healthz before failing with the journalctl hint (item 11).
	healthWait = 60 * time.Second
)

// spoondServices are the systemd unit names the phase enables and
// starts (item 10): the file names minus .service. The orchestrator is
// deliberately absent (do not restart e2b-orchestrator: its new drain
// hooks apply on the next stop/start), and so is e2b-guard (the runtime
// phase's).
var spoondServices = []string{"spoond-backend", "spoond-sshd-gateway", "spoond-dash", "spoond-drain"}

// spoondUnits are the shipped unit files the phase installs, in deploy
// embed's names: the spoond units at the top level, the drain unit and
// the orchestrator unit under e2b/. The orchestrator unit is written
// every run — it replaces the drain-less one the runtime phase wrote
// (item 10).
var spoondUnits = []struct {
	embed string
	path  string
}{

	{"spoond-dash.service", "/etc/systemd/system/spoond-dash.service"},
	{"e2b/spoond-drain.service", "/etc/systemd/system/spoond-drain.service"},
	{"e2b/e2b-orchestrator.service", orchestratorUnitPath},
}

// spoondDirs is item 2: the directories the phase needs, all 0700.
var spoondDirs = []string{"/etc/spoond", "/etc/spoond/tls", "/var/lib/spoond", "/var/lib/spoond/backups"}

// spoondPhase implements the spoond row of the phase table.
func init() { Phases[PhaseSpoond] = spoondPhase }

func spoondPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseSpoond, Out: env.Out}

	// ---- defaults (item 1) ----------------------------------------
	dd, err := spoondDefaults(ctx, env, r)
	if err != nil {
		return r.Items(), err
	}

	// ---- directories (item 2) -------------------------------------
	for _, dir := range spoondDirs {
		if err := ensureSpoondDir(env, r, dir); err != nil {
			return r.Items(), err
		}
	}

	// ---- TLS (item 4): --tls-cert (it must exist; the phase fails
	// otherwise) or a self-signed pair kept once written. Before any
	// env file, so the rendered TLS_CERT/TLS_KEY are the resolved
	// pair's paths.
	pair, err := spoondTLS(env, r, dd)
	if err != nil {
		return r.Items(), err
	}

	// ---- secrets (item 3) and backend.env (item 5): generate, or
	// read the existing backend.env's — its five token keys fill the
	// token set; the derived files take them, so all files agree.
	tk, envChanged, err := backendEnvWrite(env, r, dd, pair)
	if err != nil {
		return r.Items(), err
	}

	// ---- admin-token, drain.env, gateway env, dash (items 6, 7) ----
	if err := writeDrainFiles(env, r, dd, tk); err != nil {
		return r.Items(), err
	}
	if err := writeDashFiles(env, r, tk, pair); err != nil {
		return r.Items(), err
	}

	// ---- config-only stop (item 8) ---------------------------------
	if env.Opt.ConfigOnly && env.Opt.Container {
		return r.Items(), nil
	}

	// ---- binary copy (item 9) --------------------------------------
	binChanged := false
	if env.Opt.Container {
		r.Add(Skipped, spoondBinPath, "--container: the Docker image brings the binary")
	} else {
		binChanged, err = copyBinary(env, r)
		if err != nil {
			return r.Items(), err
		}
	}

	// ---- units (item 10) -------------------------------------------
	unitsChanged, err := spoondUnitsWrite(env, r)
	if err != nil {
		return r.Items(), err
	}
	if unitsChanged {
		if err := env.exec(ctx, "systemctl", "daemon-reload"); err != nil {
			return r.Items(), fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	if err := spoondUnitsEnable(ctx, env, r); err != nil {
		return r.Items(), err
	}
	// The restart rides the enable --now: restart spoond-backend only
	// when the binary or backend.env changed in this run and the unit
	// was active (item 10). A kept backend.env changed nothing.
	if !env.DryRun && !env.Opt.Container && (binChanged || envChanged[backendEnvPath]) {
		if err := restartBackendIfActive(ctx, env); err != nil {
			return r.Items(), err
		}
	}

	// ---- health (item 11) ------------------------------------------
	if !env.DryRun {
		if err := waitBackendHealthy(ctx, env, r, dd); err != nil {
			return r.Items(), err
		}
	}

	// ---- first admin user (item 12) ---------------------------------
	if err := firstAdminUser(ctx, env, r, tk, dd); err != nil {
		return r.Items(), err
	}

	// ---- runner (item 13) -------------------------------------------
	if env.Opt.RunnerURL != "" {
		r.Add(Skipped, "forgejo runner", "spoond install does not register a runner yet; see docs/install.md")
	} else {
		r.Add(Skipped, "forgejo runner", "not installed by default: it needs a Forgejo URL and a registration token (--runner-url, --runner-token-file); see docs/install.md")
	}

	return r.Items(), nil
}

// restartBackendIfActive restarts spoond-backend when it is running:
// the backend must pick up a changed binary or env file. An inactive
// unit is left alone (enable --now has just started it; or the
// operator stopped it on purpose).
func restartBackendIfActive(ctx context.Context, env *Env) error {
	out, err := env.query(ctx, "systemctl", "is-active", "--quiet", "spoond-backend")
	if err != nil {
		// is-active exits non-zero for inactive/unknown: not an
		// install failure, only nothing to restart.
		_ = out
		return nil
	}
	if err := env.exec(ctx, "systemctl", "restart", "spoond-backend"); err != nil {
		return fmt.Errorf("systemctl restart spoond-backend: %w", err)
	}
	return nil
}

// spoondUnitsWrite installs the shipped units through writeFile (plain
// files: comparable and correctable) with every "/forkdcache/e2b"
// replaced by the store dir, and answers whether any of them changed (a
// dry run reports the planned changes as changed rows too: writeFile's
// rows are the verdict). All paths go through env.Resolve (item 10).
func spoondUnitsWrite(env *Env, r *Reporter) (bool, error) {
	changed := false
	for _, u := range spoondUnits {
		raw, err := deploy.Files.ReadFile(u.embed)
		if err != nil {
			return false, fmt.Errorf("shipped unit %s: %w", u.embed, err)
		}
		if err := env.writeFile(r, u.path,
			[]byte(strings.ReplaceAll(string(raw), "/forkdcache/e2b", env.Opt.storeDir())),
			0o644); err != nil {
			return false, err
		}
		// writeFile reported the row; read its verdict back rather
		// than comparing again.
		last := r.Items()[len(r.Items())-1]
		if last.Name == u.path && last.Result != Kept {
			changed = true
		}
	}
	return changed, nil
}

// spoondUnitsEnable enables and starts the four spoond services (item
// 10). Rows: kept when systemd reported the unit names (a rerun), or a
// created row for the one enable --now call; failures carry systemd's
// stderr through the runner's error.
func spoondUnitsEnable(ctx context.Context, env *Env, r *Reporter) error {
	switch {
	case env.Opt.Container:
		r.Add(Skipped, "systemctl enable --now", "--container: docker compose runs the services")
		return nil
	case env.DryRun:
		r.Add(Kept, "systemctl enable --now", "dry-run: not starting "+strings.Join(spoondServices, " "))
		return nil
	}
	if err := env.exec(ctx, "systemctl", append([]string{"enable", "--now"}, spoondServices...)...); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w", strings.Join(spoondServices, " "), err)
	}
	r.Add(Kept, "systemctl enable --now", "started: "+strings.Join(spoondServices, " "))
	return nil
}

// errNoIPv4 is the host-IP default's failure with the fix (the task's
// item 1; no flag exists yet, so the fix is the env file to set).
var errNoIPv4 = errors.New("no IPv4 default route; set HOST_GUEST_SERVICE_ADDR in /etc/spoond/backend.env and rerun")

// noStoreNote is the guest-DNS note when /etc/resolv.conf has no
// usable nameserver (item 1): the guests get no resolver allowance.
const noStoreNote = "no usable nameserver in /etc/resolv.conf; guests get no DNS resolver allowance (set SPOOND_GUEST_DNS_ADDR in " + backendEnvPath + " to change that)"
