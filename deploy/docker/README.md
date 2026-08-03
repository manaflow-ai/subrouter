# Docker

The image runs as UID 65532 on a read-only root filesystem. Compose binds the proxy to loopback, caps it at 256 MiB and 256 processes, limits the Go heap to 192 MiB for native and WebSocket buffers, drops Linux capabilities, and mounts control credentials as files under `/run/secrets`.

The standalone image listens on the container interface and requires proxy, admin, and account-import secrets at `/run/secrets`. It exits before listening when any secret is absent, so a published port cannot expose an unauthenticated control plane. The Compose profiles mount the required secrets and publish only to host loopback by default.

Create local control credentials, then start local-account mode:

```bash
./deploy/docker/init-secrets.sh
docker compose -f deploy/docker/compose.yaml --profile local up --build -d
```

Import Codex or Claude credentials through the authenticated `GET` and `POST /_subrouter/account-import` flow. The container never needs SSH access or a host credential directory mount.

Team mode reads an existing cmux.com team login from one read-only secret. Copy the config without printing it:

```bash
./deploy/docker/init-secrets.sh
install -m 0600 ~/.config/subrouter/cloud.json deploy/docker/secrets/team-cloud.json
docker compose -f deploy/docker/compose.yaml --profile team up --build -d
```

The team profile selects team credential storage and the hosted cmux.com API in
memory. This lets it consume a workstation config that still names a loopback
development API or legacy routing mode without modifying the read-only secret.
Set `SUBROUTER_CLOUD_BASE_URL` on the Compose command only when testing another
cmux.com API deployment.

Run one profile at a time because both publish the same loopback port. To bind another private interface, set `SUBROUTER_BIND_IP` for the Compose command. Do not bind a public address without a network firewall.
