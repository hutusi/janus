# Deploying Janus as a Linux service

This guide runs Janus as a long-lived `systemd` service under a dedicated,
unprivileged user. It uses the ready-made unit in
[`deploy/janus.service`](../deploy/janus.service) and the secret template in
[`deploy/janus.env.example`](../deploy/janus.env.example).

For *what* each setting does, see [configuration.md](configuration.md); for
wiring a webhook, see the setup guide for your provider — [GitLab](gitlab-webhook-setup.md),
[GitHub](github-webhook-setup.md), [Gitee](gitee-webhook-setup.md), or
[GitCode](gitcode-webhook-setup.md). This page only covers turning those into a
running service.

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
service; **TLS (step 6) and the webhook (step 7) remain manual**. Prefer to
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
janus version   # janus v0.4.0 (0210cb6)
```

## 2. Create the service user

A system account with no login shell and its home at the state directory (so
`$HOME`-based caches and git config land in `/var/lib/janus`):

```sh
sudo useradd --system --home-dir /var/lib/janus --shell /usr/sbin/nologin janus
```

`/var/lib/janus` itself is created and owned automatically by the unit's
`StateDirectory=janus` — no manual `mkdir`/`chown` needed.

If a `janus` account already exists (one made with `useradd -m`, say), check its
home with `getent passwd janus`. It should be `/var/lib/janus`, because the
unit hides `/home`, and SSH clones that rely on the account's `~/.ssh` would
fail. See
[Troubleshooting](#ssh-clone-fails-known_hosts-permission-denied).
`deploy/install.sh` warns about this on install and upgrade.

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

# Secrets come from /etc/janus/janus.env (JANUS_API_TOKEN, plus a JANUS_<PROVIDER>_SECRET
# for each provider you use — see deploy/janus.env.example for the full list).

# DENY BY DEFAULT: an empty list rejects every trigger (403). List the repos or
# groups you trust, or "*" to allow all. See docs/configuration.md#repository-allowlist
allow_repos:
  - "https://gitlab.example.com/your-group"   # one entry per host/group you trust,
  - "https://github.com/your-org"             # across whichever providers you enable
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
sudo $EDITOR /etc/janus/janus.env          # set JANUS_API_TOKEN + each provider secret
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
curl -s localhost:8080/healthz  # {"status":"ok","version":"v0.4.0 (0210cb6)"}
```

`/healthz` returns `200 {"status":"ok"}` normally. It returns
`503 {"status":"degraded"}` if a run's final state could not be persisted even
after retries (a full or read-only data dir) — stored history is then stale;
check the logs and restart to clear the latch. Point your monitoring at this
endpoint.

A graceful stop exits **0** even if the HTTP shutdown deadline expires with
connections still open — one client tailing logs (`?follow=1`) is enough to
reach it, and in-flight runs are drained regardless — so `systemctl stop` does
not report a failure for that. A real shutdown error still exits non-zero.

Degraded always means the store rejected a **write**. A single unreadable run
record is not that: at startup such a run is settled `cancelled` from its
listing sidecar (logged as `settled a run whose record was unreadable`) and the
daemon stays healthy — so one corrupt file cannot wedge a liveness probe into
restarting the service forever.

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

## 7. Connect a webhook

Each provider gets its own endpoint, enabled only when its secret is set — an
unconfigured provider answers `404`. With the service reachable over HTTPS,
point the webhook at the matching path:

| Provider | Secret | Endpoint | Setup guide |
|---|---|---|---|
| GitLab | `JANUS_GITLAB_SECRET` | `/webhooks/gitlab` | [gitlab-webhook-setup.md](gitlab-webhook-setup.md) |
| GitHub | `JANUS_GITHUB_SECRET` | `/webhooks/github` | [github-webhook-setup.md](github-webhook-setup.md) |
| Gitee | `JANUS_GITEE_SECRET` | `/webhooks/gitee` | [gitee-webhook-setup.md](gitee-webhook-setup.md) |
| GitCode | `JANUS_GITCODE_SECRET` | `/webhooks/gitcode` | [gitcode-webhook-setup.md](gitcode-webhook-setup.md) |

Each guide covers which events to enable and the response codes. Remember that
`allow_repos` is deny-by-default: until it lists the repos or groups you trust,
an otherwise-valid delivery is still rejected with `403`, whichever provider
sent it.

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

### SSH clone fails: known_hosts Permission denied

With `clone_url: "ssh"`, every checkout fails (a mirror sync logs
`mirror sync failed; using direct checkout`, then the direct checkout fails
too) with:

```
hostkeys_find_by_key_hostfile: hostkeys_foreach failed for /home/janus/.ssh/known_hosts: Permission denied
Host key verification failed.
```

The `janus` account's home is under `/home`, which the unit's
`ProtectHome=true` hides from the service. ssh takes `~/.ssh` from the passwd
entry, not `$HOME`, so it cannot open its own `known_hosts`. At startup,
`janus serve` logs a warning naming the directory, and `deploy/install.sh` warns
on install and upgrade. Move the home to the state directory:

```sh
getent passwd janus                       # sixth field: /home/janus
sudo systemctl stop janus
sudo usermod -d /var/lib/janus janus
sudo install -d -m700 -o janus -g janus /var/lib/janus/.ssh
sudo cp -a /home/janus/.ssh/. /var/lib/janus/.ssh/
sudo chown -R janus:janus /var/lib/janus/.ssh
# also carry over /home/janus/.gitconfig (safe.directory etc.) if there is one
```

Before starting the service again, make sure `known_hosts` has the git
server's key, and that you have **verified** the key: once a key is in that
file, every checkout trusts it. Scan into a scratch file, compare the
fingerprints with the ones your git server publishes (its admin docs or admin),
and append only after they match. On a non-default SSH port the entry is keyed
`[host]:port`, so scan with the port:

```sh
hk=$(mktemp)                              # private (0600), unpredictable name
ssh-keyscan -p 2222 git.example.com > "$hk"
ssh-keygen -lf "$hk"                      # compare every fingerprint before going on
sudo -u janus tee -a /var/lib/janus/.ssh/known_hosts < "$hk" >/dev/null
rm -f "$hk"
sudo systemctl start janus
```

Then verify inside the sandbox, as described in
[SSH clone URLs](configuration.md#ssh-clone-urls) (`systemd-run … -p ProtectHome=true
git ls-remote …`). Don't relax the unit to `ProtectHome=read-only` instead: that
re-exposes every user's home to pipeline steps and leaves `$HOME` unwritable.

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
- **The service account's home must be `/var/lib/janus`**, which stays writable
  under the sandbox, so tool caches, `git config`, and ssh's `~/.ssh` work.
  `/home` and `/root` are hidden. Because ssh resolves `~/.ssh` from the passwd
  entry rather than `$HOME`, an account whose home is under `/home` can't use
  a key or `known_hosts` there for SSH clones
  ([Troubleshooting](#ssh-clone-fails-known_hosts-permission-denied)).
- **Stay behind the loopback bind + proxy** from step 3 so the unprotected
  dashboard and read `/api` endpoints are not exposed directly.
- **The `allow_repos` allowlist is your last line of defence** if a webhook
  secret or API token leaks — keep it as tight as possible.
- **Graceful stop**: `KillMode=mixed` sends `SIGTERM` to the main process only,
  so Janus drains runs and kills each step's process group itself;
  `TimeoutStopSec=60` covers its grace period before systemd `SIGKILL`s the rest.

To inspect the effective sandbox on your host: `systemd-analyze security janus`.
