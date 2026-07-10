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

## Quick install (scripted)

If you just want it running, [`deploy/install.sh`](../deploy/install.sh) automates
steps 1–5 below from a checkout:

```sh
git clone https://github.com/hutusi/janus && cd janus
sudo ./deploy/install.sh --allow-repo https://gitlab.example.com/your-group
```

It detects your architecture, downloads and **checksum-verifies** the latest
release binary, creates the `janus` user, writes `/etc/janus/janus.yml` and a
`janus.env` with **freshly generated secrets (printed once)**, installs the unit,
and starts the service. It is **idempotent** — an existing config or secrets file
is kept, never overwritten — so it is safe to re-run. Useful flags:

- `--dry-run` — print every action without changing anything (needs no root).
- `--version vX.Y.Z` — pin a release instead of `latest`.
- `--binary <path>` — install a locally-built binary with **no network access at
  all** (air-gapped hosts); see [Offline install](#offline--air-gapped-install) below.
- `--no-gen-secrets` — leave `janus.env` blank to fill in yourself.
- `sudo ./deploy/install.sh upgrade` — re-download + verify the binary and
  restart, leaving the user, config and secrets untouched (see [step 8](#8-upgrading)).

Run `./deploy/install.sh --help` for the full list. The script stops at a running
service; **TLS (step 6) and the GitLab webhook (step 7) remain manual**. Prefer to
understand or customise each step? Follow the manual walkthrough below instead.

### Offline / air-gapped install

The only thing `install.sh` fetches from the internet is the release binary. On a
host with no internet, build the binary yourself and hand it to the installer with
`--binary` — it then skips the download **and** the remote checksum, so the whole
install runs entirely from local files (the unit and secret templates already come
from the checkout, and secrets are generated with `openssl`):

```sh
# On the air-gapped host (needs Go and this checkout):
make build                                       # produces ./janus
sudo ./deploy/install.sh --binary ./janus \
  --allow-repo https://gitlab.internal/your-group
```

`make install-service` is shorthand for the same thing —
`make build && make install-service INSTALL_FLAGS="--allow-repo https://gitlab.internal/your-group"`.
(For just the binary on PATH with no service, use `make install`.)

`upgrade` takes it too — rebuild, then
`sudo ./deploy/install.sh upgrade --binary ./janus`. If the target has no Go,
cross-compile on a connected box with `GOOS=linux GOARCH=amd64 make build` (use
`GOARCH=arm64` for aarch64 hosts) and copy the resulting `janus` plus the `deploy/`
directory over before running the installer.

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

### "Connection refused" from other machines

By design: the service binds **`127.0.0.1:8080`** (the `addr` in
`/etc/janus/janus.yml`), so `curl 127.0.0.1:8080` works on the host while
`http://<server-ip>:8080` is refused from anywhere else. Only local clients —
like the reverse proxy above — can reach it; browse it through the proxy's
`https://` name, not port 8080.

To expose it directly on a trusted network instead, set `addr: "0.0.0.0:8080"`
in `/etc/janus/janus.yml`, run `sudo systemctl restart janus`, and open the port
in your firewall / cloud security group ("refused" becoming "timeout" means the
firewall is the remaining blocker). Understand what that trades away: the
dashboard is not behind the API token, and the token and webhook secret then
travel as plain HTTP. Note that re-running `install.sh --addr …` does **not**
change an existing install — the installer never overwrites an existing config;
edit the file.

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

## 9. Uninstalling

```sh
sudo ./deploy/install.sh uninstall      # or, from a checkout: make uninstall-service
```

Stops and disables the service, removes the unit and the binary — and **keeps**
`/etc/janus` (config + secrets), `/var/lib/janus` (run history), and the `janus`
user, so nothing is destroyed by surprise and a later reinstall picks the config
back up. To remove those too:

```sh
sudo ./deploy/install.sh uninstall --purge
```

Both variants support `--dry-run` to preview every action first.

## Troubleshooting

### git "dubious ownership" when triggering a local repository

A trigger whose `repo_url` is a **local path** can fail with:

```
exit status 128
fatal: detected dubious ownership in repository at '...'
fatal: Could not read from remote repository.
```

The service runs as the dedicated `janus` user, and git refuses to read a
repository owned by a different user (CVE-2022-24765) — the "remote" error is
just how the local-transport fetch surfaces that refusal. The exception git
suggests must be added **as the service user** (it lives in that user's
`~/.gitconfig`, i.e. `/var/lib/janus/.gitconfig`), not as root or the repo owner:

```sh
sudo -u janus -H git config --global --add safe.directory /path/to/repo
# a linked worktree resolves into the parent repo — add the gitdir git names too:
sudo -u janus -H git config --global --add safe.directory '/path/to/repo/.git/worktrees/<name>'
# or, blanket for the janus user only (the repo allowlist still gates triggers):
sudo -u janus -H git config --global --add safe.directory '*'
```

The `janus` user also needs plain read/traverse permission on the path, and
repositories under `/home` are unreachable regardless — the unit's
`ProtectHome` blocks them by design. Triggering via the git server URL instead
of a local path avoids all of this.

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
