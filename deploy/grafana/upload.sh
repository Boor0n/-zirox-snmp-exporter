#!/bin/sh
# Uploads the Zirox dashboards to Grafana through its API, into the folder
# "Zirox". Running it again replaces them with the current version.
#
#   curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/grafana/upload.sh |
#     GRAFANA_URL=http://grafana:3000 GRAFANA_TOKEN=glsa_... sh
#
# GRAFANA_TOKEN is a service account token with the Editor role
# (Administration -> Users and access -> Service accounts). Instead of a token,
# GRAFANA_USER and GRAFANA_PASSWORD work too. ZIROX_VERSION=vX.Y.Z takes the
# dashboards of that release instead of main; ZIROX_DASHBOARDS=dir takes them
# from a local directory. Behind a proxy set https_proxy for GitHub and
# no_proxy for Grafana.
set -eu

REPO=${ZIROX_REPO:-Boor0n/-zirox-snmp-exporter}
REF=${ZIROX_VERSION:-main}
URL=${GRAFANA_URL:?set GRAFANA_URL, e.g. http://127.0.0.1:3000}
URL=${URL%/}
FOLDER_UID=zirox

die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

if [ -n "${GRAFANA_TOKEN:-}" ]; then
	auth="Authorization: Bearer $GRAFANA_TOKEN"
elif [ -n "${GRAFANA_USER:-}" ]; then
	auth="Authorization: Basic $(printf '%s:%s' "$GRAFANA_USER" "${GRAFANA_PASSWORD:-}" | base64 | tr -d '\n')"
else
	die "set GRAFANA_TOKEN (or GRAFANA_USER and GRAFANA_PASSWORD)"
fi

grafana() { # method path [body file]
	if [ $# -ge 3 ]; then
		curl -sS -X "$1" -H "$auth" -H "Content-Type: application/json" --data-binary "@$3" "$URL$2"
	else
		curl -sS -X "$1" -H "$auth" "$URL$2"
	fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

grafana GET /api/folders/$FOLDER_UID >"$tmp/folder" || die "cannot reach Grafana at $URL"
if ! grep -q "\"uid\":\"$FOLDER_UID\"" "$tmp/folder"; then
	printf '{"uid":"%s","title":"Zirox"}' "$FOLDER_UID" >"$tmp/f.json"
	grafana POST /api/folders "$tmp/f.json" >"$tmp/out"
	grep -q "\"uid\":\"$FOLDER_UID\"" "$tmp/out" || die "cannot create the folder: $(cat "$tmp/out")"
fi

for name in zirox-noc zirox-device zirox-port zirox-health; do
	if [ -n "${ZIROX_DASHBOARDS:-}" ]; then
		cp "$ZIROX_DASHBOARDS/$name.json" "$tmp/$name.json" || die "no $name.json in $ZIROX_DASHBOARDS"
	else
		curl -fsSL -o "$tmp/$name.json" \
			"https://raw.githubusercontent.com/$REPO/$REF/deploy/grafana/$name.json" ||
			die "cannot download $name.json ($REF)"
	fi
	{
		printf '{"folderUid":"%s","overwrite":true,"message":"zirox upload.sh %s","dashboard":' "$FOLDER_UID" "$REF"
		cat "$tmp/$name.json"
		printf '}'
	} >"$tmp/body.json"
	grafana POST /api/dashboards/db "$tmp/body.json" >"$tmp/out"
	grep -q '"status":"success"' "$tmp/out" || die "upload of $name failed: $(cat "$tmp/out")"
	say "Uploaded $name: $URL/d/$name"
done
say "Open $URL/d/zirox-noc and pick the Prometheus (or VictoriaMetrics) data source at the top."
