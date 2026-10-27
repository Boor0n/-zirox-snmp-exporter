#!/bin/sh
# One-command install or upgrade of Zirox SNMP Exporter from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh
#
# Private repository: pass a GitHub token that can read it, both to curl and
# to the script:
#
#   curl -fsSL -H "Authorization: Bearer $T" -H "Accept: application/vnd.github.raw" \
#     https://api.github.com/repos/Boor0n/-zirox-snmp-exporter/contents/deploy/get.sh | sudo GITHUB_TOKEN=$T sh
#
# Behind a proxy, give it to curl (-x) and to the script (https_proxy=...).
#
# Options (after "sh -s --", e.g. "| sudo sh -s -- --systemd"):
#   --docker            run in Docker (default when Docker is running)
#   --systemd           install the binary as a systemd service
#   --version vX.Y.Z    install this release instead of the latest
#   --port N            web UI and /probe port (default 9116)
#   --uninstall         remove the container and the service, keep the data
#
# Data (database and encryption key) is kept in /var/lib/zirox and settings in
# /etc/zirox/zirox.env, so running the command again upgrades in place.
set -eu

REPO=${ZIROX_REPO:-Boor0n/-zirox-snmp-exporter}
API=${ZIROX_API:-https://api.github.com/repos/$REPO}
IMAGE=zirox-snmp-exporter
DATA_DIR=/var/lib/zirox
ENV_DIR=/etc/zirox
ENV_FILE=$ENV_DIR/zirox.env
CONTAINER=zirox

mode="" version="" port=9116 uninstall=false
while [ $# -gt 0 ]; do
	case "$1" in
	--docker) mode=docker; shift ;;
	--systemd) mode=systemd; shift ;;
	--version) version=${2:?--version needs a value}; shift 2 ;;
	--port) port=${2:?--port needs a value}; shift 2 ;;
	--uninstall) uninstall=true; shift ;;
	-h|--help) sed -n '2,20p' "$0" 2>/dev/null || echo "see deploy/get.sh in $REPO"; exit 0 ;;
	*) echo "error: unknown option $1" >&2; exit 1 ;;
	esac
done

die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

[ "$(id -u)" -eq 0 ] || die "run as root: ... | sudo sh"
command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"

docker_ok() { command -v docker >/dev/null && docker info >/dev/null 2>&1; }

if $uninstall; then
	if docker_ok && docker inspect "$CONTAINER" >/dev/null 2>&1; then
		docker rm -f "$CONTAINER" >/dev/null && say "Removed container $CONTAINER"
	fi
	if command -v systemctl >/dev/null && [ -f /etc/systemd/system/zirox.service ]; then
		systemctl disable --now zirox 2>/dev/null || true
		rm -f /etc/systemd/system/zirox.service /usr/local/bin/zirox
		systemctl daemon-reload
		say "Removed systemd service zirox"
	fi
	say "Data is kept in $DATA_DIR and $ENV_DIR; delete them by hand if no longer needed."
	exit 0
fi

if [ -z "$mode" ]; then
	if docker_ok; then mode=docker; else mode=systemd; fi
fi
case "$mode" in
docker) docker_ok || die "Docker is not running; start it or use --systemd" ;;
systemd) command -v systemctl >/dev/null || die "systemd not found; install Docker and use --docker" ;;
esac

case "$(uname -m)" in
x86_64|amd64) arch=amd64 ;;
aarch64|arm64) arch=arm64 ;;
*) die "unsupported CPU $(uname -m); builds exist for amd64 and arm64" ;;
esac

# api GETs a GitHub API path, with the token when one is given.
api() {
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		curl -fsSL -H "Authorization: token $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" "$API$1"
	else
		curl -fsSL -H "Accept: application/vnd.github+json" "$API$1"
	fi
}

# download saves a release asset by its API id (works for private repositories).
download() {
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		curl -fsSL -H "Authorization: token $GITHUB_TOKEN" -H "Accept: application/octet-stream" -o "$2" "$API/releases/assets/$1"
	else
		curl -fsSL -H "Accept: application/octet-stream" -o "$2" "$API/releases/assets/$1"
	fi
}

if [ -n "$version" ]; then
	release=$(api "/releases/tags/$version") || die "release $version not found in $REPO"
else
	release=$(api /releases/latest) || die "cannot read releases of $REPO (private repository? set GITHUB_TOKEN)"
fi
tag=$(printf '%s' "$release" | tr ',' '\n' | sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
[ -n "$tag" ] || die "cannot read the release tag"

# asset_id prints the id of the release asset with the given name.
asset_id() {
	printf '%s' "$release" | tr ',{}' '\n\n\n' | awk -v want="$1" '
		/"url": *"[^"]*\/releases\/assets\/[0-9]+"/ { n = $0; sub(/.*\/releases\/assets\//, "", n); sub(/".*/, "", n); id = n }
		/"name": *"/ { v = $0; sub(/^[^:]*: *"/, "", v); sub(/".*/, "", v); if (v == want && id != "") { print id; exit } }'
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ "$mode" = docker ]; then
	file=zirox-image-$tag-linux-$arch.tar.gz
else
	file=zirox-$tag-linux-$arch.tar.gz
fi
id=$(asset_id "$file")
[ -n "$id" ] || die "release $tag has no $file"
say "Downloading $file ($tag)"
download "$id" "$tmp/$file" || die "download failed"
sums=$(asset_id SHA256SUMS)
if [ -n "$sums" ] && command -v sha256sum >/dev/null; then
	download "$sums" "$tmp/SHA256SUMS" || die "download of SHA256SUMS failed"
	(cd "$tmp" && grep " $file\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "checksum of $file does not match"
fi

# The admin password is generated once and kept across upgrades.
password=""
if [ ! -f "$ENV_FILE" ]; then
	mkdir -p "$ENV_DIR"
	chmod 0750 "$ENV_DIR"
	password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-16)
	cat >"$ENV_FILE" <<EOF
# Zirox SNMP Exporter settings, see README for all options.
# The admin password is only used on the first start; change it later in Settings.
ZIROX_ADMIN_USER=admin
ZIROX_ADMIN_PASSWORD=$password
#ZIROX_SCRAPE_TOKEN=
#ZIROX_COOKIE_SECURE=1
#ZIROX_MAX_POLLS=128
#ZIROX_MAX_DISCOVERY=32
EOF
	chmod 0640 "$ENV_FILE"
fi

if [ "$mode" = docker ]; then
	if command -v systemctl >/dev/null && systemctl is-active --quiet zirox 2>/dev/null; then
		die "the systemd service zirox is running; remove it first: ... | sudo sh -s -- --uninstall"
	fi
	say "Loading the Docker image"
	docker load -i "$tmp/$file" >/dev/null
	docker tag "$IMAGE:$tag" "$IMAGE:latest"
	mkdir -p "$DATA_DIR"
	# The image runs as uid 10001; -R also takes over files left by a
	# systemd install.
	chown -R 10001:10001 "$DATA_DIR"
	chmod 0700 "$DATA_DIR"
	upgrade=false
	if docker inspect "$CONTAINER" >/dev/null 2>&1; then
		upgrade=true
		docker rm -f "$CONTAINER" >/dev/null
	fi
	# On SELinux hosts (RHEL, Rocky, Alma) the data directory needs a
	# container label, otherwise Zirox cannot write its database.
	vol=$DATA_DIR:/data
	if command -v getenforce >/dev/null && [ "$(getenforce 2>/dev/null)" != Disabled ]; then
		vol=$vol:Z
	fi
	docker run -d --name "$CONTAINER" --restart unless-stopped \
		-p "$port:9116" --env-file "$ENV_FILE" -e ZIROX_LISTEN=:9116 \
		-v "$vol" "$IMAGE:$tag" >/dev/null
	logs="docker logs -f $CONTAINER"
else
	if docker_ok && docker inspect "$CONTAINER" >/dev/null 2>&1; then
		die "the Docker container $CONTAINER exists; remove it first: ... | sudo sh -s -- --uninstall"
	fi
	tar -xzf "$tmp/$file" -C "$tmp"
	dir=$tmp/zirox-$tag-linux-$arch
	if grep -q '^ZIROX_LISTEN=' "$ENV_FILE"; then
		sed -i "s/^ZIROX_LISTEN=.*/ZIROX_LISTEN=:$port/" "$ENV_FILE"
	else
		echo "ZIROX_LISTEN=:$port" >>"$ENV_FILE"
	fi
	upgrade=false
	[ -f /usr/local/bin/zirox ] && upgrade=true
	# install.sh creates the zirox user, the unit and the data directory.
	sh "$dir/install.sh" --binary "$dir/zirox" >/dev/null
	chgrp zirox "$ENV_FILE" 2>/dev/null || true
	logs="journalctl -u zirox -f"
fi

say "Waiting for the web UI"
ok=false
for _ in $(seq 1 30); do
	if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then ok=true; break; fi
	sleep 1
done
$ok || die "Zirox did not answer on port $port; see: $logs"

host=$(hostname -f 2>/dev/null || hostname)
if $upgrade; then
	say "Upgraded to $tag ($mode)."
else
	say "Installed $tag ($mode)."
fi
echo "    Web UI:     http://$host:$port"
if [ -n "$password" ]; then
	echo "    Login:      admin / $password  (saved in $ENV_FILE)"
fi
echo "    Prometheus: http://$host:$port/sd (HTTP service discovery)"
echo "    Logs:       $logs"
echo "    Upgrade:    run the same command again"
echo "    Back up $DATA_DIR (database and secret.key) and $ENV_FILE."
