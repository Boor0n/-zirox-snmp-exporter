#!/bin/sh
# Upgrade test: an older release creates a database with a device, then the
# binary built from this tree opens it. The device, its credential and its
# ticks must survive, polling must work, and a backup must be written.
#
#   deploy/ci/upgrade-test.sh v0.3.0 ./zirox-new
#
# Needs an SNMP agent on 127.0.0.1:1161 answering community "testcomm"
# (CI starts snmpd), curl and tar.
set -eu

FROM=${1:?usage: upgrade-test.sh FROM_VERSION NEW_BINARY}
NEW=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
REPO=${ZIROX_REPO:-Boor0n/-zirox-snmp-exporter}
PORT=19116
URL=http://127.0.0.1:$PORT

work=$(mktemp -d)
pid=""
cleanup() {
	[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	echo "--- log" >&2
	tail -n 40 "$work/log" >&2 || true
	exit 1
}

start() { # binary
	ZIROX_DATA_DIR=$work/data ZIROX_LISTEN=127.0.0.1:$PORT ZIROX_ADMIN_PASSWORD=upgrade-test-pw \
		ZIROX_UPDATE_CHECK=0 "$1" >>"$work/log" 2>&1 &
	pid=$!
	for _ in $(seq 1 50); do
		curl -fsS "$URL/healthz" >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	fail "$1 did not start"
}
stop() {
	kill "$pid"
	wait "$pid" 2>/dev/null || true
	pid=""
}
login() {
	rm -f "$work/jar"
	curl -fsS -c "$work/jar" -H "Origin: $URL" -o /dev/null \
		--data-urlencode user=admin --data-urlencode password=upgrade-test-pw "$URL/login" || fail "login"
}
post() { # path, then --data-urlencode args
	p=$1
	shift
	curl -fsS -b "$work/jar" -H "Origin: $URL" -o /dev/null "$@" "$URL$p"
}
get() { curl -fsS -b "$work/jar" "$URL$1"; }
wait_probe() { # what to wait for in /probe?target=1
	for _ in $(seq 1 90); do
		get "/probe?target=1" 2>/dev/null | grep -q "$1" && return 0
		sleep 1
	done
	fail "no '$1' in /probe: $(get '/probe?target=1' | head -20)"
}

echo "==> Downloading $FROM"
asset=zirox-$FROM-linux-amd64
curl -fsSL "https://github.com/$REPO/releases/download/$FROM/$asset.tar.gz" | tar -xz -C "$work"
old=$work/$asset/zirox
[ -x "$old" ] || fail "no binary in $asset.tar.gz"

echo "==> $FROM: add a device"
start "$old"
login
post /devices/new --data-urlencode name=ci-snmpd --data-urlencode address=127.0.0.1 --data-urlencode port=1161 \
	--data-urlencode enabled=on --data-urlencode credential_id=new --data-urlencode cred_name=ci-upgrade-cred \
	--data-urlencode version=2c --data-urlencode community=testcomm --data-urlencode profile_id=0 ||
	fail "add device"
# The first poll can come before discovery has found the ports: wait for them.
wait_probe '^ifOperStatus'
before=$(get "/probe?target=1" | grep -c '^ifOperStatus' || true)
[ "$before" -gt 0 ] || fail "$FROM polls no ports"
stop

echo "==> $(basename "$NEW"): open the same data"
start "$NEW"
login
get /devices | grep -q ci-snmpd || fail "device lost"
get /credentials | grep -q ci-upgrade-cred || fail "credential lost"
wait_probe '^ifOperStatus'
after=$(get "/probe?target=1" | grep -c '^ifOperStatus' || true)
[ "$after" -eq "$before" ] || fail "polled ports changed: $before before, $after after"
ls "$work/data/backups/"*.db >/dev/null 2>&1 || fail "no backup written before the upgrade"
if grep -q "panic" "$work/log"; then fail "panic in the log"; fi

# The new version must still refresh and keep the ticks.
post /devices/1/refresh || fail "refresh"
sleep 3
wait_probe 'zirox_poll_success 1'
stop
echo "OK: $FROM -> new: device, credential, $after ports and polling survived; backup in data/backups"
