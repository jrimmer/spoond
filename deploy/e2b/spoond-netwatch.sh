#!/bin/bash
# spoond-netwatch — recover the host's network when its receive path dies.
#
# On 2026-10-02 the host's 10 GbE port stopped receiving (RX 0 from
# 15:06:30) while the kernel kept the link up and kept sending; nothing
# reached the host for 50 minutes and a hard reset lost every lease.
# This loop probes the LAN and, when every target stops answering:
#   1. after FAIL_AFTER s: log the port's state, bounce the physical port;
#   2. STEP_WAIT s later, still down: ifreload -a;
#   3. still down after REBOOT_AFTER s in total, the port received nothing
#      in that time, and no netwatch reboot in REBOOT_MIN_GAP s: a clean
#      `systemctl reboot` (spoond-drain drains leases first).
# A port that still receives means the fault is upstream (router, switch);
# rebooting this host cannot fix that, so step 3 is skipped.
#
# Settings (environment, /etc/default/spoond-netwatch):
#   NETWATCH_IF       physical port to bounce        (default enp1s0f0)
#   NETWATCH_BRIDGE   bridge that holds the address  (default vmbr0)
#   NETWATCH_TARGETS  space-separated probe targets  (required; e.g. "10.0.0.1 10.0.0.2")
#   NETWATCH_INTERVAL probe interval, s              (default 15)
#   FAIL_AFTER, STEP_WAIT, REBOOT_AFTER, REBOOT_MIN_GAP  (120, 60, 600, 43200)
#   NETWATCH_DRYRUN   1 = log the actions, take none
set -uo pipefail
IF=${NETWATCH_IF:-enp1s0f0}
BR=${NETWATCH_BRIDGE:-vmbr0}
TARGETS=${NETWATCH_TARGETS:-}
if [ -z "$TARGETS" ]; then
  echo "netwatch: NETWATCH_TARGETS is required (space-separated probe targets, e.g. \"10.0.0.1 10.0.0.2\")" >&2
  exit 2
fi
INTERVAL=${NETWATCH_INTERVAL:-15}
FAIL_AFTER=${FAIL_AFTER:-120}
STEP_WAIT=${STEP_WAIT:-60}
REBOOT_AFTER=${REBOOT_AFTER:-600}
REBOOT_MIN_GAP=${REBOOT_MIN_GAP:-43200}
DRYRUN=${NETWATCH_DRYRUN:-0}
STATE=/var/lib/spoond-netwatch
mkdir -p "$STATE"

say() { echo "netwatch: $*"; }
act() { if [ "$DRYRUN" = 1 ]; then say "dry run, would: $*"; else say "doing: $*"; "$@"; fi; }
up() { local t; for t in $TARGETS; do ping -c 1 -W 2 -I "$BR" "$t" >/dev/null 2>&1 && return 0; done; return 1; }
rx() { cat "/sys/class/net/$IF/statistics/rx_packets" 2>/dev/null || echo 0; }
snapshot() {
  say "state of $IF ($1):"
  { ip -s link show "$IF"; ethtool "$IF" | grep -E 'Speed|Duplex|Link detected'; ethtool -S "$IF" | grep -v ': 0$'; ip neigh show dev "$BR"; } 2>&1 | sed 's/^/netwatch:   /'
}

say "watching $IF (bridge $BR), targets: $TARGETS, every ${INTERVAL}s$([ "$DRYRUN" = 1 ] && echo ', DRY RUN')"
down_since=0 rx_at_down=0 step=0
while true; do
  now=$(date +%s)
  if up; then
    if [ "$down_since" != 0 ]; then say "network back after $(( now - down_since ))s (step $step)"; fi
    down_since=0 step=0
  else
    if [ "$down_since" = 0 ]; then
      down_since=$now rx_at_down=$(rx) step=0
      say "no answer from $TARGETS; watching"
    fi
    down=$(( now - down_since ))
    if [ "$step" = 0 ] && [ "$down" -ge "$FAIL_AFTER" ]; then
      snapshot "down ${down}s"
      act ip link set "$IF" down; sleep 2; act ip link set "$IF" up
      step=1 step_at=$now
    elif [ "$step" = 1 ] && [ $(( now - step_at )) -ge "$STEP_WAIT" ]; then
      snapshot "down ${down}s, after the port bounce"
      act ifreload -a
      step=2 step_at=$now
    elif [ "$step" = 2 ] && [ "$down" -ge "$REBOOT_AFTER" ]; then
      got=$(( $(rx) - rx_at_down ))
      last=$(cat "$STATE/last-reboot" 2>/dev/null || echo 0)
      if [ "$got" -gt 0 ]; then
        say "still down after ${down}s but $IF received $got packets: the fault is upstream; not rebooting"
      elif [ $(( now - last )) -lt "$REBOOT_MIN_GAP" ]; then
        say "still down after ${down}s; last netwatch reboot $(( now - last ))s ago (< ${REBOOT_MIN_GAP}s); not rebooting"
      else
        snapshot "down ${down}s, $IF received nothing; clean reboot"
        [ "$DRYRUN" = 1 ] || echo "$now" > "$STATE/last-reboot"
        act systemctl reboot
      fi
      step=3
    fi
  fi
  sleep "$INTERVAL"
done
