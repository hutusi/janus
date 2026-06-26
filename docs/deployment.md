# Deploying Janus as a Linux service

This guide runs Janus as a long-lived `systemd` service under a dedicated,
unprivileged user. It uses the ready-made unit in
[`deploy/janus.service`](../deploy/janus.service) and the secret template in
[`deploy/janus.env.example`](../deploy/janus.env.example).

For *what* each setting does, see [configuration.md](configuration.md); for
wiring a webhook, see [gitlab-webhook-setup.md](gitlab-webhook-setup.md). This
page only covers turning those into a running service.

## Prerequisites

- A Linux host with `systemd` (any mainstream distro).
- **`git`** installed — Janus shells out to it to check out the triggered commit.
- Whatever toolchains your pipelines invoke (`node`, `go`, `python`, …) installed
  on the host. Janus runs steps as **host processes**; it does not provide them.
- Read the [security model](../README.md#security-model-read-this-before-deploying)
  first: jobs run with no isolation, so the dedicated user and the `allow_repos`
  allowlist below are not optional extras.

## 1. Install the binary

Download the release binary for your architecture, verify it, and place it on the
`PATH` (see the [README install section](../README.md#install) for arm64/checksum
details):

```sh
curl -fsSL -o /tmp/janus \
  https://github.com/hutusi/janus/releases/latest/download/janus-linux-amd64
sudo install -m755 /tmp/janus /usr/local/bin/janus
janus version   # janus v0.1.0
```

## 2. Create the service user

A system account with no login shell and its home at the state directory (so
`$HOME`-based caches and git config land in `/var/lib/janus`):

```sh
sudo useradd --system --home-dir /var/lib/janus --shell /usr/sbin/nologin janus
```

`/var/lib/janus` itself is created and owned automatically by the unit's
`StateDirectory=janus` — no manual `mkdir`/`chown` needed.

## 3. Configure

Create the config directory and write `/etc/janus/janus.yml`. Start from
[`internal/config/example.yml`](../internal/config/example.yml) and switch to
absolute paths. A production-shaped config:

```yaml
# /etc/janus/janus.yml — full key reference: docs/configuration.md
addr: "127.0.0.1:8080"               # bind to loopback; terminate TLS at a proxy (step 7)
data_dir: "/var/lib/janus"            # persistent run history
workspace_root: "/var/lib/janus/workspaces"
step_timeout: "30m"                   # fail a step that runs longer than this

# Secrets come from /etc/janus/janus.env (JANUS_API_TOKEN, JANUS_GITLAB_SECRET).

# DENY BY DEFAULT: an empty list rejects every trigger (403). List the repos or
# groups you trust, or "*" to allow all. See docs/configuration.md#repository-allowlist
allow_repos:
  - "https://gitlab.example.com/your-group"
```

The config holds no secrets, so make it readable by the janus user but not the
world:

```sh
sudo mkdir -p /etc/janus
sudo $EDITOR /etc/janus/janus.yml
sudo chown root:janus /etc/janus/janus.yml && sudo chmod 640 /etc/janus/janus.yml
```

Then install the secrets file from the template, fill it in, and lock it down
(systemd reads it as root, so it stays `root:root`):

```sh
sudo install -m600 deploy/janus.env.example /etc/janus/janus.env
sudo $EDITOR /etc/janus/janus.env          # set JANUS_API_TOKEN / JANUS_GITLAB_SECRET
```

Generate strong values with `openssl rand -hex 32` (API token) and
`openssl rand -hex 24` (webhook secret).

## 4. Install and start the unit

```sh
sudo install -m644 deploy/janus.service /etc/systemd/system/janus.service
sudo systemctl daemon-reload
sudo systemctl enable --now janus
```

## 5. Verify

```sh
systemctl status janus
journalctl -u janus -f          # follow logs
curl -s localhost:8080/healthz  # {"status":"ok","version":"v0.1.0"}
```

## 6. TLS / reverse proxy

Janus speaks **plain HTTP** and has no TLS of its own — terminate TLS at a reverse
proxy and forward to the loopback address from step 3. With [Caddy](https://caddyserver.com/),
that is a one-liner that also provisions a certificate:

```caddy
ci.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

nginx works equally well (`proxy_pass http://127.0.0.1:8080;`). See the
[reverse-proxy note](configuration.md#config-file) in the configuration docs.

## 7. Connect a GitLab webhook

With the service reachable over HTTPS and `JANUS_GITLAB_SECRET` set, point a
GitLab webhook at `https://ci.example.com/webhooks/gitlab`. Full steps —
including which events to enable and the response codes — are in
[gitlab-webhook-setup.md](gitlab-webhook-setup.md).

## 8. Upgrading

Replace the binary and restart; the SIGTERM-driven drain (see hardening notes)
lets in-flight runs finish or be cleanly cancelled:

```sh
curl -fsSL -o /tmp/janus \
  https://github.com/hutusi/janus/releases/latest/download/janus-linux-amd64
sudo install -m755 /tmp/janus /usr/local/bin/janus
sudo systemctl restart janus
janus version
```

## Hardening notes

The shipped unit applies a **balanced** sandbox (`NoNewPrivileges`,
`ProtectSystem=full`, `ProtectHome`, `PrivateTmp`, kernel-tunable/cgroup
protection). Because pipeline steps are arbitrary host commands, keep these in
mind:

- **`NoNewPrivileges=true` blocks `sudo`** inside pipeline steps. That is
  intentional — a CI job should not escalate. Drop it only if a trusted pipeline
  genuinely needs it.
- **`$HOME` is `/var/lib/janus`** (the service user's home), which stays writable
  under the sandbox, so tool caches and `git config` work. `/home` and `/root`
  are hidden.
- **Stay behind the loopback bind + proxy** from step 3 so the unprotected
  dashboard and read `/api` endpoints are not exposed directly.
- **The `allow_repos` allowlist is your last line of defence** if a webhook
  secret or API token leaks — keep it as tight as possible.
- **Graceful stop**: `KillMode=mixed` sends `SIGTERM` to the main process only,
  so Janus drains runs and kills each step's process group itself;
  `TimeoutStopSec=60` covers its grace period before systemd `SIGKILL`s the rest.

To inspect the effective sandbox on your host: `systemd-analyze security janus`.
