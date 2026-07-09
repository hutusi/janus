#!/bin/sh
# Janus scripted installer — provision Janus as a hardened systemd service.
#
# Automates steps 1-5 of docs/deployment.md on a Linux systemd host: downloads
# and checksum-verifies the release binary, creates the dedicated `janus` user,
# installs the config + secret templates (idempotently — never clobbers existing
# ones), installs the systemd unit, and enables the service.
#
# For an air-gapped host, build the binary first (`make build`) and pass it with
# --binary <path>; the install then makes no network access at all.
#
# TLS/reverse-proxy (deployment.md step 6) and the GitLab webhook (step 7) stay
# manual and are pointed to at the end.
#
# Usage:
#   sudo ./deploy/install.sh [install|upgrade] [options]
#   sudo ./deploy/install.sh --binary ./janus   # offline: install a local build
#   ./deploy/install.sh --dry-run               # preview, no root or Linux needed
# Run --help for the option list, or read docs/deployment.md for the walkthrough.

set -eu

# --- constants -------------------------------------------------------------
REPO_URL="https://github.com/hutusi/janus"
USER_NAME="janus"
STATE_DIR="/var/lib/janus"
CONFIG_DIR="/etc/janus"
UNIT_PATH="/etc/systemd/system/janus.service"

# Directory this script lives in, so we can read the sibling janus.service /
# janus.env.example that ship alongside it (keeps the installed unit in lockstep
# with the committed one — no embedded copy to drift).
SCRIPT_DIR=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd)

# --- defaults (overridable by flags) --------------------------------------
MODE="install"
VERSION="latest"
ADDR="127.0.0.1:8080"
PREFIX="/usr/local/bin"
GEN_SECRETS=1
DRY_RUN=0
ALLOW_REPOS=""   # newline-separated, built up from repeated --allow-repo
ARCH=""
BINARY=""        # local binary to install (offline); empty = download a release

# --- helpers ---------------------------------------------------------------
log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
err()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# run: echo a command, then execute it — or, under --dry-run, only echo it.
# Every state-changing command goes through here so --dry-run touches nothing.
run() {
	printf '+ %s\n' "$*"
	if [ "$DRY_RUN" -eq 0 ]; then
		"$@"
	fi
}

usage() {
	cat <<EOF
Janus scripted installer — provision Janus as a systemd service.

Usage: sudo ./deploy/install.sh [install|upgrade] [options]

Options:
  --allow-repo <url>   allow a repo/group URL prefix (repeatable; seeds allow_repos)
  --binary <path>      install this local binary and make no network access at all
                       (build it first with 'make build'); skips --version
  --version <vX.Y.Z>   install a specific release (default: latest)
  --addr <host:port>   listen address (default: 127.0.0.1:8080)
  --prefix <dir>       install the binary here (default: /usr/local/bin)
  --no-gen-secrets     do not auto-generate secrets; leave janus.env blank
  --dry-run            print the planned actions without changing anything
  -h, --help           show this help

install (default)      full provision: binary, user, config, secrets, unit, start
upgrade                re-download + checksum-verify the binary, then restart
                       (leaves the user, config and secrets untouched)

Automates steps 1-5 of docs/deployment.md. TLS and the GitLab webhook stay
manual — see that guide.
EOF
}

detect_arch() {
	m=$(uname -m)
	case "$m" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*) err "unsupported architecture: $m (supported: x86_64, aarch64)" ;;
	esac
}

require_tools() {
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || err "required tool not found: $t"
	done
}

check_preconditions() {
	# --dry-run is a preview: skip host/root/tool gating so it runs anywhere
	# (e.g. a macOS dev box). Only reflect openssl availability in the preview.
	if [ "$DRY_RUN" -eq 1 ]; then
		if [ "$GEN_SECRETS" -eq 1 ] && ! command -v openssl >/dev/null 2>&1; then
			GEN_SECRETS=0
		fi
		return
	fi

	[ "$(uname -s)" = "Linux" ] || err "this installer targets Linux systemd hosts (found $(uname -s))"
	[ "$(id -u)" = "0" ] || err "must run as root — re-run with: sudo $0 $MODE ..."
	[ -d /run/systemd/system ] || err "systemd not detected (/run/systemd/system is missing)"

	# curl/sha256sum are only needed to download + verify a release; a local
	# --binary install needs neither, keeping air-gapped hosts happy.
	if [ -n "$BINARY" ]; then
		require_tools install systemctl
	else
		require_tools curl sha256sum install systemctl
	fi
	if [ "$MODE" = "install" ]; then
		require_tools useradd
		if [ "$GEN_SECRETS" -eq 1 ] && ! command -v openssl >/dev/null 2>&1; then
			warn "openssl not found; leaving secrets blank — set them in $CONFIG_DIR/janus.env"
			GEN_SECRETS=0
		fi
		command -v git >/dev/null 2>&1 ||
			warn "git not found — Janus shells out to git to check out commits; install it before running pipelines"
	fi
}

# Put the janus binary in place: from a local --binary (offline) or, by default,
# by downloading and checksum-verifying a release.
provision_binary() {
	if [ -n "$BINARY" ]; then
		if [ "$DRY_RUN" -eq 0 ] && [ ! -f "$BINARY" ]; then
			err "--binary not found or not a regular file: $BINARY"
		fi
		log "installing local binary (offline, no download): $BINARY"
		run install -m755 "$BINARY" "$PREFIX/janus"
		log "installed $PREFIX/janus"
		return
	fi
	download_binary
}

download_binary() {
	detect_arch
	asset="janus-linux-$ARCH"
	if [ "$VERSION" = "latest" ]; then
		base="$REPO_URL/releases/latest/download"
	else
		base="$REPO_URL/releases/download/$VERSION"
	fi

	if [ "$DRY_RUN" -eq 1 ]; then
		tmp="/tmp/janus-install.XXXXXX" # placeholder for the preview only
	else
		tmp=$(mktemp -d)
		trap 'rm -rf "$tmp"' EXIT
	fi

	log "downloading $asset ($VERSION)"
	run curl -fsSL -o "$tmp/$asset" "$base/$asset"
	run curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

	# sha256sum -c must run from the directory holding the listed files.
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '+ (cd %s && sha256sum -c checksums.txt --ignore-missing)\n' "$tmp"
	else
		(cd "$tmp" && sha256sum -c checksums.txt --ignore-missing) ||
			err "checksum verification failed for $asset"
	fi

	run install -m755 "$tmp/$asset" "$PREFIX/janus"
	log "installed $PREFIX/janus"
}

create_user() {
	if id "$USER_NAME" >/dev/null 2>&1; then
		log "user '$USER_NAME' already exists"
		return
	fi
	run useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$USER_NAME"
}

# Emit the allow_repos YAML block from the collected --allow-repo values, or a
# clearly-marked placeholder when none were given.
format_allow_repos() {
	if [ -n "$ALLOW_REPOS" ]; then
		printf '%s\n' "$ALLOW_REPOS" | while IFS= read -r r; do
			[ -n "$r" ] && printf '  - "%s"\n' "$r"
		done
	else
		printf '  - "https://gitlab.example.com/your-group"   # PLACEHOLDER — edit me\n'
	fi
}

install_config() {
	if [ -e "$CONFIG_DIR/janus.yml" ]; then
		log "kept existing config $CONFIG_DIR/janus.yml"
		return
	fi
	[ -n "$ALLOW_REPOS" ] ||
		warn "no --allow-repo given: allow_repos is a placeholder, so Janus will reject every trigger (403) until you edit $CONFIG_DIR/janus.yml"

	if [ "$DRY_RUN" -eq 1 ]; then
		log "would write $CONFIG_DIR/janus.yml (allow_repos: ${ALLOW_REPOS:-<placeholder>})"
		return
	fi

	repos_block=$(format_allow_repos)
	tmp_cfg=$(mktemp)
	cat >"$tmp_cfg" <<EOF
# /etc/janus/janus.yml — written by deploy/install.sh.
# Full key reference: docs/configuration.md
addr: "$ADDR"                          # loopback bind; terminate TLS at a proxy
data_dir: "$STATE_DIR"                 # persistent run history
workspace_root: "$STATE_DIR/workspaces"
step_timeout: "30m"                    # fail a step that runs longer than this

# Secrets come from $CONFIG_DIR/janus.env (JANUS_API_TOKEN, JANUS_GITLAB_SECRET).

# DENY BY DEFAULT: an empty list rejects every trigger (403). List the repos or
# groups you trust, or "*" to allow all. See docs/configuration.md#repository-allowlist
allow_repos:
$repos_block
EOF
	install -m640 -g "$USER_NAME" "$tmp_cfg" "$CONFIG_DIR/janus.yml"
	rm -f "$tmp_cfg"
	log "wrote $CONFIG_DIR/janus.yml"
}

install_env() {
	if [ -e "$CONFIG_DIR/janus.env" ]; then
		log "kept existing secrets $CONFIG_DIR/janus.env"
		return
	fi
	src="$SCRIPT_DIR/janus.env.example"
	[ -f "$src" ] || err "missing $src — run this script from a Janus checkout"

	if [ "$DRY_RUN" -eq 1 ]; then
		if [ "$GEN_SECRETS" -eq 1 ]; then
			log "would write $CONFIG_DIR/janus.env with generated JANUS_API_TOKEN / JANUS_GITLAB_SECRET"
		else
			log "would write $CONFIG_DIR/janus.env from template (blank secrets)"
		fi
		return
	fi

	tmp_env=$(mktemp)
	if [ "$GEN_SECRETS" -eq 1 ]; then
		api=$(openssl rand -hex 32)
		hook=$(openssl rand -hex 24)
		# hex values contain no sed-special characters, so plain substitution is safe.
		sed -e "s#^JANUS_API_TOKEN=.*#JANUS_API_TOKEN=$api#" \
			-e "s#^JANUS_GITLAB_SECRET=.*#JANUS_GITLAB_SECRET=$hook#" \
			"$src" >"$tmp_env"
	else
		cat "$src" >"$tmp_env"
	fi
	install -m600 "$tmp_env" "$CONFIG_DIR/janus.env"
	rm -f "$tmp_env"
	log "wrote $CONFIG_DIR/janus.env"

	if [ "$GEN_SECRETS" -eq 1 ]; then
		printf '\n'
		log "generated secrets — store these now, they are shown once:"
		printf '    JANUS_API_TOKEN=%s\n' "$api"
		printf '    JANUS_GITLAB_SECRET=%s\n\n' "$hook"
	fi
}

install_unit() {
	src="$SCRIPT_DIR/janus.service"
	[ -f "$src" ] || err "missing $src — run this script from a Janus checkout"
	run install -m644 "$src" "$UNIT_PATH"
	run systemctl daemon-reload
}

verify_service() {
	log "verifying service"
	run systemctl is-active janus || true
	url="http://$ADDR/healthz"
	if [ "$DRY_RUN" -eq 1 ]; then
		printf "+ curl -fsS --noproxy '*' %s\n" "$url"
		return
	fi
	# --noproxy '*': the bind is loopback, so never route the check through an
	# http_proxy the host may have set (it would 502 a perfectly healthy service).
	i=0
	while [ "$i" -lt 10 ]; do
		if out=$(curl -fsS --noproxy '*' "$url" 2>/dev/null); then
			log "healthz: $out"
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	warn "healthz did not respond at $url yet — inspect with: journalctl -u janus -e"
}

print_summary() {
	cat <<EOF

Janus is installed and running as a systemd service.

Next steps (not automated — see docs/deployment.md):
  * TLS: put a reverse proxy in front of $ADDR                (step 6)
  * GitLab webhook: point it at https://<host>/webhooks/gitlab (step 7)

Handy commands:
  systemctl status janus
  journalctl -u janus -f
  curl -s http://$ADDR/healthz
EOF
}

# --- argument parsing ------------------------------------------------------
case "${1:-}" in
install | upgrade)
	MODE="$1"
	shift
	;;
esac

while [ "$#" -gt 0 ]; do
	case "$1" in
	--allow-repo)
		shift
		[ "$#" -gt 0 ] || err "--allow-repo requires a URL"
		if [ -z "$ALLOW_REPOS" ]; then
			ALLOW_REPOS="$1"
		else
			ALLOW_REPOS="$ALLOW_REPOS
$1"
		fi
		;;
	--binary)
		shift
		[ "$#" -gt 0 ] || err "--binary requires a path"
		BINARY="$1"
		;;
	--version)
		shift
		[ "$#" -gt 0 ] || err "--version requires a value"
		VERSION="$1"
		;;
	--addr)
		shift
		[ "$#" -gt 0 ] || err "--addr requires a value"
		ADDR="$1"
		;;
	--prefix)
		shift
		[ "$#" -gt 0 ] || err "--prefix requires a value"
		PREFIX="$1"
		;;
	--no-gen-secrets) GEN_SECRETS=0 ;;
	--dry-run) DRY_RUN=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) err "unknown argument: $1 (see --help)" ;;
	esac
	shift
done

# --- main ------------------------------------------------------------------
if [ -n "$BINARY" ] && [ "$VERSION" != "latest" ]; then
	err "--binary and --version are mutually exclusive (--version only selects a download)"
fi

check_preconditions

if [ "$PREFIX" != "/usr/local/bin" ]; then
	warn "the shipped unit runs /usr/local/bin/janus; with --prefix $PREFIX you must edit ExecStart in $UNIT_PATH"
fi

provision_binary

if [ "$MODE" = "upgrade" ]; then
	install_unit
	run systemctl restart janus
	verify_service
	log "upgrade complete"
	exit 0
fi

create_user
run install -d -m755 "$CONFIG_DIR"
install_config
install_env
install_unit
run systemctl enable --now janus
verify_service
print_summary
