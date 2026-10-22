#!/bin/sh
# Installs or upgrades Zirox SNMP Exporter as a systemd service.
#
#   sudo ./install.sh                  install or upgrade (binary next to this script)
#   sudo ./install.sh --binary PATH    use a specific binary
#   sudo ./install.sh --uninstall      remove the service, keep data in /var/lib/zirox
#
# Data lives in /var/lib/zirox (database and encryption key), settings in
# /etc/zirox/zirox.env. Both are kept on upgrade and uninstall.
set -eu

BIN=/usr/local/bin/zirox
UNIT=/etc/systemd/system/zirox.service
ENV_DIR=/etc/zirox
ENV_FILE=$ENV_DIR/zirox.env
DATA_DIR=/var/lib/zirox
DIR=$(cd "$(dirname "$0")" && pwd)

die() { echo "error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0)"
command -v systemctl >/dev/null || die "systemd is required; use Docker otherwise (see README)"

binary=""
while [ $# -gt 0 ]; do
	case "$1" in
	--binary) binary=${2:?--binary needs a path}; shift 2 ;;
	--uninstall)
		systemctl disable --now zirox 2>/dev/null || true
		rm -f "$UNIT" "$BIN"
		systemctl daemon-reload
		echo "Removed. Data is kept in $DATA_DIR and $ENV_DIR; delete them by hand if no longer needed."
		exit 0 ;;
	-h|--help) sed -n '2,10p' "$0"; exit 0 ;;
	*) die "unknown option $1" ;;
	esac
done

# Find the binary: --binary, next to this script (release archive), or build
# from the source tree this script lives in.
if [ -z "$binary" ]; then
	for c in "$DIR/zirox" "$DIR/../zirox"; do
		[ -f "$c" ] && binary=$c && break
	done
fi
if [ -z "$binary" ] && [ -f "$DIR/../go.mod" ] && command -v go >/dev/null; then
	echo "Building from source..."
	(cd "$DIR/.." && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o zirox ./cmd/zirox)
	binary=$DIR/../zirox
fi
[ -n "$binary" ] || die "no zirox binary found; download the release archive or pass --binary PATH"
"$binary" -version >/dev/null 2>&1 || die "$binary does not run on this machine (wrong architecture?)"

unit_src=""
for c in "$DIR/zirox.service" "$DIR/deploy/zirox.service"; do
	[ -f "$c" ] && unit_src=$c && break
done
[ -n "$unit_src" ] || die "zirox.service not found next to $0"

if ! id zirox >/dev/null 2>&1; then
	useradd --system --no-create-home --home-dir "$DATA_DIR" --shell /usr/sbin/nologin zirox
fi

upgrade=false
[ -f "$BIN" ] && upgrade=true
install -m 0755 "$binary" "$BIN.new"
mv -f "$BIN.new" "$BIN"
install -m 0644 "$unit_src" "$UNIT"

password=""
if [ ! -f "$ENV_FILE" ]; then
	install -d -m 0750 -g zirox "$ENV_DIR"
	password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-16)
	cat >"$ENV_FILE" <<EOF
# Zirox SNMP Exporter settings, see README for all options.
# The admin password is only used on the first start; change it later in Settings.
ZIROX_ADMIN_USER=admin
ZIROX_ADMIN_PASSWORD=$password
ZIROX_LISTEN=:9116
#ZIROX_SCRAPE_TOKEN=
#ZIROX_COOKIE_SECURE=1
EOF
	chown root:zirox "$ENV_FILE"
	chmod 0640 "$ENV_FILE"
fi

systemctl daemon-reload
systemctl enable zirox >/dev/null 2>&1
systemctl restart zirox
sleep 2
systemctl is-active --quiet zirox || { journalctl -u zirox -n 30 --no-pager; die "service did not start"; }

version=$("$BIN" -version)
if $upgrade; then
	echo "Upgraded to $version and restarted."
else
	echo "Installed $version."
fi
port=$(sed -n 's/^ZIROX_LISTEN=.*:\([0-9]*\)$/\1/p' "$ENV_FILE")
echo "Web UI: http://$(hostname -f 2>/dev/null || hostname):${port:-9116}"
if [ -n "$password" ]; then
	echo "Login: admin / $password  (also saved in $ENV_FILE)"
fi
echo "Logs: journalctl -u zirox -f"
echo "Back up $DATA_DIR/secret.key together with $DATA_DIR/zirox.db."
