// Package deploy ships the systemd units, env examples and E2B config
// files spoond installs. `spoond install` embeds them (deploy/embed.go)
// and writes exactly what the repo ships, so the installed files and the
// ones documented in deploy/README.md never drift.
package deploy

import (
	"embed"
)

// Files holds the shipped units, env examples and e2b config files:
// the spoond units at the top level (backend, gateway, runner, dash,
// sshd-gateway), their env examples, and under e2b/ the orchestrator
// unit and env, the e2b-guard firewall and unit, flags.json, the
// netwatch unit and script, and the otelcol config.
//
// Patterns mirror what the installer writes for each phase; a file not
// listed here is not written by `spoond install`.
//
//go:embed *.service *.example e2b/*.service e2b/*.nft e2b/orchestrator.env e2b/flags.json e2b/spoond-netwatch.sh e2b/otelcol-config.yaml
var Files embed.FS