# forkd-backend deployment

The lease API runs as a systemd service on vm2 (10.1.0.11), alongside
forkd-controller.

## Build

```bash
go build -o forkd-backend ./cmd/forkd-backend
```

## Deploy

```bash
scp forkd-backend root@10.1.0.11:/opt/forkd-backend/
scp deploy/forkd-backend.service root@10.1.0.11:/etc/systemd/system/
```

On vm2, create `/etc/forkd-backend.env` with the consumer tokens:

```bash
cat > /etc/forkd-backend.env <<'EOF'
CONSUMER_TOKENS=<token>=<consumer>,<token2>=<consumer2>
POOL_SIZE=3
EOF
chmod 600 /etc/forkd-backend.env
```

`POOL_SIZE` pre-forks that many sandboxes per image so grants are served
from the warm pool (milliseconds) instead of cold-spawning. 0 disables
the pool (default).

Then:

```bash
systemctl daemon-reload
systemctl enable --now forkd-backend
systemctl status forkd-backend
```

## Verify

```bash
# health: list images
curl -s -H "Authorization: Bearer <token>" http://127.0.0.1:8890/api/images

# create a sandbox
curl -s -X POST -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"image":"py-base","ttl":300}' \
  http://127.0.0.1:8890/api/sandboxes

# exec into it
curl -s -X POST -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"python3 -c \"print(2+2)\""}' \
  http://127.0.0.1:8890/api/sandboxes/<id>/exec
```

## TLS

To serve HTTPS, set `TLS_CERT` and `TLS_KEY` in `/etc/forkd-backend.env`
and point them at a cert/key pair. The default bind is `127.0.0.1:8890`;
expose it only behind a TLS reverse proxy for non-localhost consumers.
