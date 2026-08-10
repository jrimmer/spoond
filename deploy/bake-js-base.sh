#!/bin/bash
# Bake js-base snapshot: node:22 + git (ticket #17 U3). Retry with fixed args.
set -e
cd /var/cache/forkd
export FORKD_SCRIPTS_DIR=/usr/local/share/forkd-scripts
# from-image registers the tag; a stale tag is overwritten by re-registration.
# (There is no snapshot --delete; skip cleanup entirely.)
forkd from-image node:22 --tag js-base --extra 'git ca-certificates curl' --size-mib 3072 2>&1 | tail -6
echo "=== verify images ==="
forkd images 2>&1 | grep -E 'js-base|TAG'
# Spawn + verify node/git resolve
SB=$(curl -s -X POST http://127.0.0.1:8889/v1/sandboxes -H 'Content-Type: application/json' -d '{"snapshot_tag":"js-base","n":1,"per_child_netns":true}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0]['id'] if isinstance(d,list) and d else d.get('id',''))")
echo "sandbox: $SB"
if [ -z "$SB" ]; then echo "SPAWN FAILED"; exit 1; fi
sleep 8
curl -s -X POST "http://127.0.0.1:8889/v1/sandboxes/$SB/exec" -H 'Content-Type: application/json' -d '{"args":["bash","-c","node --version && git --version && echo JS_BASE_OK"]}' | python3 -c "import sys,json; d=json.load(sys.stdin); print('exec out:', d.get('stdout',''))"
curl -s -X DELETE "http://127.0.0.1:8889/v1/sandboxes/$SB" >/dev/null
echo "BAKE DONE"
