#!/usr/bin/env bash
# The spoond image's entrypoint (deploy/docker/compose.yaml runs one
# service per mode). Each mode reads the same files the systemd units
# read, from the bind-mounted config, and runs the same command.
#
#   orchestrator  load the e2b-guard rules, run the E2B orchestrator;
#                 drain on stop, undrain once healthy (the unit's hooks)
#   backend       spoond backend
#   gateway       spoond gateway (the SSH gateway)
#   dash          spoond dash (the dashboard)
#   init          the runtime and spoond phases' config into /config
#   anything else /opt/spoond/spoond with that command line, with the
#                 backend's environment when it exists (doctor, images,
#                 install, ctl, ...)
set -euo pipefail

SPOOND=/opt/spoond/spoond

# load_env reads a systemd EnvironmentFile: KEY=VALUE lines, comments and
# blanks skipped, one pair of surrounding quotes stripped. Values are
# taken literally (no shell expansion: a bcrypt hash keeps its $).
load_env() {
	local file=$1 line key val
	[ -f "$file" ] || return 0
	while IFS= read -r line || [ -n "$line" ]; do
		line=${line#"${line%%[![:space:]]*}"}
		case "$line" in '' | '#'* | ';'*) continue ;; esac
		case "$line" in *=*) ;; *) continue ;; esac
		key=${line%%=*}
		val=${line#*=}
		case "$val" in
		\"*\") val=${val#\"} val=${val%\"} ;;
		\'*\') val=${val#\'} val=${val%\'} ;;
		esac
		export "$key=$val"
	done <"$file"
}

backend_env() {
	export BIND_ADDR=127.0.0.1:8890
	load_env /etc/spoond/llm.env
	load_env /etc/spoond/backend.env
}

wait_healthy() {
	local url=$1 tries=$2
	for _ in $(seq 1 "$tries"); do
		curl -fsSk -o /dev/null "$url" && return 0
		sleep 2
	done
	return 1
}

orchestrator() {
	load_env /etc/e2b/orchestrator.env
	load_env /etc/e2b/drain.env
	local store=${SPOOND_STORE_DIR:-/forkdcache/e2b}

	# The orchestrator's FORWARD rules must land in the backend the
	# host's Docker uses: its FORWARD policy is DROP, and a rule in the
	# other backend does not override it. The image defaults to legacy
	# (a native host's choice); a host whose Docker chains live in
	# iptables-nft (Debian 13's default) switches the image to nft.
	if iptables-nft-save 2>/dev/null | grep -q '^:DOCKER' &&
		! iptables-legacy-save 2>/dev/null | grep -q '^:DOCKER'; then
		update-alternatives --quiet --set iptables /usr/sbin/iptables-nft
		update-alternatives --quiet --set ip6tables /usr/sbin/ip6tables-nft
		echo "spoond-container: the host's Docker uses iptables-nft; so does the orchestrator"
	fi

	# e2b-guard.service: the shipped table deletes and re-adds itself.
	nft -f /etc/nftables.d/e2b-guard.nft
	# The unit's ExecStartPre lines.
	iptables -w 5 -t mangle -C FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null ||
		iptables -w 5 -t mangle -I FORWARD 1 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
	# cgroup v2: the orchestrator enables cpu and memory for its own
	# e2b cgroup, which needs them enabled at the container's root, and
	# a cgroup with processes cannot enable them. Move this container's
	# processes into a child first (the Docker-in-Docker step).
	if [ -f /sys/fs/cgroup/cgroup.controllers ] && ! grep -qw memory /sys/fs/cgroup/cgroup.subtree_control; then
		mkdir -p /sys/fs/cgroup/init
		xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
		sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
	fi
	mkdir -p "$store/tmp" /fc-vm
	ulimit -n 1048576
	ulimit -l unlimited

	/usr/local/lib/e2b/orchestrator &
	local pid=$!

	# ExecStop: drain first, so the leases suspend before the
	# orchestrator goes (compose's stop_grace_period covers it).
	stopping() {
		trap - TERM INT
		echo "spoond-container: draining before the orchestrator stops"
		"$SPOOND" drain --stop || echo "spoond-container: drain --stop failed; stopping anyway"
		kill -TERM "$pid" 2>/dev/null || true
		wait "$pid" || true
		nft delete table inet e2b_guard 2>/dev/null || true
		exit 0
	}
	trap stopping TERM INT

	# ExecStartPost and spoond-drain.service: undrain once the
	# orchestrator and the backend both answer.
	(
		wait_healthy http://127.0.0.1:5008/health 210 &&
			wait_healthy https://127.0.0.1:8890/healthz 180 &&
			"$SPOOND" drain --start
	) || echo "spoond-container: drain --start did not run; run it by hand once the backend is up" &

	local rc=0
	wait "$pid" || rc=$?
	exit "$rc"
}

init() {
	local store=${SPOOND_STORE_DIR:-/forkdcache/e2b}
	"$SPOOND" install runtime --container --root=/config --store-dir="$store"
	"$SPOOND" install spoond --container --config-only --root=/config --store-dir="$store" \
		${SPOOND_HOST_NAME:+--host-name="$SPOOND_HOST_NAME"}
}

case "${1:-help}" in
init)
	init
	;;
orchestrator)
	orchestrator
	;;
backend)
	backend_env
	exec "$SPOOND" backend
	;;
gateway)
	load_env /etc/spoond-gateway.env
	mkdir -p /etc/spoond-gateway/keys
	exec "$SPOOND" gateway \
		--listen :2222 \
		--host-key /etc/spoond-gateway/ssh_host_ed25519_key \
		--gateway-key /etc/spoond-gateway/gateway_ed25519 \
		--client-keys /etc/spoond-gateway/keys \
		--backend https://127.0.0.1:8890 \
		--gateway-host "${SPOOND_GATEWAY_HOST:?SPOOND_GATEWAY_HOST is not set in /etc/spoond-gateway.env}"
	;;
dash)
	load_env /etc/spoond/dash.env
	exec "$SPOOND" dash
	;;
*)
	backend_env
	exec "$SPOOND" "$@"
	;;
esac
