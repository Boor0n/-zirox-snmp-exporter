# Zirox SNMP Exporter

An SNMP exporter for Prometheus with a web UI. You add devices in the browser. When a device is added, it is walked **once in full** to find its ports, CPU, memory, temperature sensors, power supplies and fans. After that only the **ticked** objects are polled on schedule, with targeted SNMP GETs and no walks. Prometheus reads the values from the exporter's memory and never talks to the devices.

## How it works

1. **Add / Refresh.** A full walk of `ifTable`/`ifXTable`, CISCO-PROCESS-MIB, CISCO-MEMORY-POOL-MIB, CISCO-SYSTEM-EXT-MIB (Nexus), ENTITY-SENSOR-MIB, CISCO-ENVMON-MIB, HOST-RESOURCES-MIB, UCD-SNMP-MIB, CISCO-FIREWALL-MIB and CISCO-REMOTE-ACCESS-MONITOR-MIB (ASA), and BGP4-MIB.
2. **Select.** The device page lists every object with a checkbox. By default physical ports that are up and all health objects are ticked. Ports that are down and VLAN interfaces are not.
3. **Poll.** Only ticked objects, in batches of up to 30 OIDs per GET. For a 48-port switch with 10 ticked ports that is 4 small requests per interval.
4. **Refresh.** A new full walk, on demand or on schedule (Settings → Scheduled refresh, e.g. weekly at night; off by default). New ports are highlighted and ticked right away if they are up (configurable). Missing ones are marked and no longer polled. Your ticks are kept: ports are matched by name (`ifName`), not by `ifIndex`.

### Poll interval

The interval is set in seconds at three levels; the most specific wins:

| Level | Where |
| --- | --- |
| Device | Device page → Edit → Custom interval |
| Poll profile | Poll profiles (defaults: Fast 6 s, Normal 60 s, Slow 300 s) |
| Global | Settings (default 60 s) |

Changes apply immediately, without a restart. The minimum is 5 s. If a device does not answer, its interval is stretched for a while (2×, 4×… up to 10 minutes) so an unreachable host is not hammered. At most 128 polls and 32 full walks run at the same time (`ZIROX_MAX_POLLS`, `ZIROX_MAX_DISCOVERY`). These limit concurrent work, not the number of devices: other devices wait a fraction of a second for their turn. See "How many devices" below.

## Install

### One command

On a Linux server with Docker (without Docker it installs a systemd service instead):

```bash
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh
```

Behind a proxy, give it to both `curl` and the script (Docker downloads nothing during the install; the image comes as a file):

```bash
P=http://proxy:8080
curl -fsSL -x "$P" https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo https_proxy=$P sh
```

From a private fork, create a fine-grained GitHub token with Contents: Read-only on that repository and fetch the script through the API (raw.githubusercontent.com answers 404 for fine-grained tokens on private repositories):

```bash
T=github_pat_...
curl -fsSL -H "Authorization: Bearer $T" -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/OWNER/REPO/contents/deploy/get.sh | sudo GITHUB_TOKEN=$T ZIROX_REPO=OWNER/REPO sh
```

The script downloads the latest release, verifies its SHA256, loads the image into Docker and starts the `zirox` container (`--restart unless-stopped`), waits for the web UI and prints its address and the admin password. Data lives in `/var/lib/zirox`, settings in `/etc/zirox/zirox.env`. On SELinux hosts (RHEL, Rocky, Alma) the data directory is labelled for the container automatically.

- Upgrade: run the same command again. The database, key and settings are kept.
- Options go after `sh -s --`, e.g. `... | sudo sh -s -- --port 9200`:
  - `--systemd` install the binary as a systemd service instead of Docker (the default when Docker is not running);
  - `--version v0.3.0` a specific release instead of the latest;
  - `--port N` port of the web UI and `/probe` (default 9116);
  - `--uninstall` remove the container or the service; data is kept.

### Docker from source

```bash
docker compose up -d --build
docker logs zirox   # if ZIROX_ADMIN_PASSWORD is empty, the generated admin password is printed here
```

Open `http://<server>:9116`.

### Without Docker (systemd)

Download `zirox-<version>-linux-amd64.tar.gz` from the Releases page (`linux-arm64` for ARM servers), then:

```bash
tar -xzf zirox-*-linux-amd64.tar.gz && cd zirox-*-linux-amd64
sudo ./install.sh
```

The script installs the binary to `/usr/local/bin/zirox`, creates the `zirox` user, the systemd service and the settings file `/etc/zirox/zirox.env` with a random admin password (printed at the end), and starts the service. Data lives in `/var/lib/zirox`.

- Upgrade: unpack the new archive and run `sudo ./install.sh` again. The database, key and settings are kept.
- Remove: `sudo ./install.sh --uninstall` (data is kept).
- From source: `sudo deploy/install.sh` in a clone builds the binary itself (needs Go 1.26+).

A release is built automatically when a tag is pushed (`git tag v0.2.0 && git push origin v0.2.0`) or with the button on GitHub: Actions → release → Run workflow, with the version.

### Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `ZIROX_LISTEN` | `:9116` | HTTP listen address |
| `ZIROX_DATA_DIR` | `./data` | SQLite database and encryption key |
| `ZIROX_ADMIN_USER` | `admin` | Administrator login (created on first start) |
| `ZIROX_ADMIN_PASSWORD` | random | Administrator password on first start |
| `ZIROX_RESET_ADMIN_PASSWORD` | — | `1` = reset the password to `ZIROX_ADMIN_PASSWORD` on start |
| `ZIROX_SECRET_KEY` | file `data/secret.key` | 32 bytes in base64 to encrypt communities and v3 passwords |
| `ZIROX_SCRAPE_TOKEN` | — | Require `Authorization: Bearer <token>` on `/probe`, `/sd`, `/metrics` |
| `ZIROX_SD_ADDRESS` | Host of the request | `host:port` Prometheus uses to reach the exporter |
| `ZIROX_COOKIE_SECURE` | — | `1` if the UI is served over HTTPS |
| `ZIROX_TLS_CERT`, `ZIROX_TLS_KEY` | — | Serve HTTPS directly from the exporter |
| `ZIROX_MAX_POLLS` | `128` | Concurrent polls |
| `ZIROX_MAX_DISCOVERY` | `32` | Concurrent full walks |

**Back up `data/secret.key`** together with the database: without the key the stored communities and passwords cannot be decrypted.

## How many devices

There is no limit on the number of devices. Every device has its own poll loop; `ZIROX_MAX_POLLS` and `ZIROX_MAX_DISCOVERY` only limit how many requests run at once. Load test (`ZIROX_SCALE=1000 ZIROX_SCALE_PORTS=200 go test ./internal/engine -run TestScale -v`): switches with half of their ports up and ticked, 20 ms network latency, 10 s interval. Memory is for Zirox itself, without the simulated devices.

| Devices | Ports per device | Ports total | Full walk of all | All polled within the interval | Memory |
|---|---|---|---|---|---|
| 300 | 48 | 14,400 | 6 s | yes | 17 MB |
| 1000 | 48 | 48,000 | 16 s | yes | 60 MB |
| 1000 | 200 | 200,000 | 17 s | yes | 240 MB |
| 1000 | 500 | 500,000 | 27 s | yes | 650 MB |

Memory grows by about 1.3 KB per ticked port. With 100 ms latency and 1000 devices of 48 ports, the full walk of all takes about 1 min 17 s and regular polling still fits the interval.

With hundreds of thousands of ports the bottleneck is Prometheus, not Zirox: each ticked port gives about 11 series. For example, 250,000 ticked ports give about 2.7 million series, and Prometheus needs a few kilobytes of memory per series. Tick only the ports you need, use 60 s for access ports and faster intervals only for uplinks, and use VictoriaMetrics or Mimir for long-term storage.

For several thousand devices or slow links, raise `ZIROX_MAX_POLLS` (e.g. 256) in `/etc/zirox/zirox.env` and restart with the same install command.

## Prometheus

```yaml
scrape_configs:
  - job_name: zirox-snmp
    http_sd_configs:
      - url: http://zirox-host:9116/sd
        refresh_interval: 60s
```

Through HTTP service discovery Prometheus gets the list of devices and the interval of each (`__scrape_interval__`). A device added in the web UI shows up in Prometheus without editing the config. Each device is a separate target with the labels `instance`, `device`, `group`, `address`, `sysName`.

| Endpoint | Returns |
| --- | --- |
| `/sd` | Targets for HTTP SD (JSON) |
| `/probe?target=<id\|name\|IP>` | Metrics of one device from the cache |
| `/metrics/all` | All devices in one response, with `device` and `group` labels |
| `/metrics` | The exporter's own state |
| `/healthz` | Liveness check |

### Metrics

Ports (same names as the standard snmp_exporter, so existing Grafana dashboards work): `ifHCInOctets`, `ifHCOutOctets`, `ifHCInUcastPkts`, `ifHCOutUcastPkts`, `ifInErrors`, `ifOutErrors`, `ifInDiscards`, `ifOutDiscards`, `ifOperStatus`, `ifAdminStatus`, `ifHighSpeed` with the labels `ifIndex`, `ifName`, `ifDescr`, `ifAlias`. For SNMPv1, `ifInOctets`/`ifOutOctets`/`ifSpeed` replace the 64-bit counters.

Device health: `zirox_cpu_utilization_percent{window}`, `zirox_memory_used_bytes`, `zirox_memory_free_bytes`, `zirox_memory_utilization_percent`, `zirox_memory_total_bytes`, `zirox_memory_available_bytes`, `zirox_temperature_celsius`, `zirox_sensor_value{unit}`, `zirox_sensor_status`, `zirox_envmon_{temperature,fan,psu}_state`, `zirox_storage_{size,used}_bytes`.

Firewalls (ASA): `zirox_firewall_connections{type="current"|"max"}`, `zirox_vpn_sessions{type="all"|"l2l"|"anyconnect"}`, `zirox_vpn_users`.

BGP peers (BGP4-MIB, IPv4): `zirox_bgp_peer_state` (6 = established), `zirox_bgp_peer_admin_status`, `zirox_bgp_peer_established_seconds`, `zirox_bgp_peer_updates_received_total` with the labels `peer`, `remote_as`.

Internal: `zirox_poll_success`, `zirox_poll_duration_seconds`, `zirox_poll_last_success_timestamp_seconds`, `zirox_poll_interval_seconds`, `zirox_poll_samples`, `zirox_device_info`.

Example queries:

```promql
rate(ifHCInOctets{device="core-nx-01"}[5m]) * 8   # inbound traffic, bit/s
increase(ifInErrors[1h]) > 0                        # ports with errors in the last hour
zirox_poll_success == 0                             # devices that do not answer
```

### Alerts

`deploy/alerts.yml` has ready Prometheus rules: device not answering for 5 minutes, CPU > 90 %, memory > 90 %, temperature > 70 °C, PSU/fan/sensor problem (CISCO-ENVMON), disk > 90 % full, port down while admin up, port errors, port utilization > 90 %, BGP peer not established. Adjust the thresholds to your needs.

```yaml
rule_files:
  - /etc/prometheus/zirox-alerts.yml
```

Check: `promtool check rules deploy/alerts.yml && promtool test rules deploy/alerts_test.yml`.

## Grafana

`deploy/grafana-dashboard.json` is the "Zirox SNMP" dashboard. Import: Grafana → Dashboards → New → Import → upload the file, pick the Prometheus data source. Filters at the top: group, device, interface. Sections:

- **Overview**: number of devices, how many do not answer, ports down, hardware problems, a table of all devices with the time of the last successful poll;
- **Health**: CPU, memory, temperature, disks, PSU and fan state;
- **Interfaces**: inbound/outbound traffic, utilization in %, errors and discards, a table of ports with status and speed;
- **Firewall and routing**: ASA connections and VPN sessions, a table of BGP peers.

Since the port metric names match the standard snmp_exporter, ready dashboards from the Grafana catalog work too.

## CSV import

Import takes a CSV with a header. Only the `address` column is required. Optional: `name`, `port`, `group`, `profile` (profile name), `credential` (name of saved credentials) or `version` + `community` for v1/v2c.

```csv
name,address,group,profile,credential
core-nx-01,10.0.0.1,DC1,Fast (6 s),Nexus RO v3
acc-sw-12,10.0.1.12,DC1,,
```

## Custom OIDs

If an OID is not among the built-in ones (ports, CPU, memory, sensors, disks, firewall, VPN, BGP), add it on the **Custom OIDs** page:

1. In "Check an OID on a device", pick a device and an OID. Turn on **Walk** to see a whole table or subtree. You see right away what the device returns and which values are numeric.
2. Click **Add column** to poll a table column (one value per row: per port, sensor, pool), or **Add value** for a single value (usually an OID ending in `.0`).
3. Set the metric name (e.g. `cisco_mempool_used_bytes`), the type (gauge or counter) and, if needed, a multiplier. For a table you can name a column with row names; it becomes the `name` label.
4. Click **Refresh all devices** or Refresh on a device. Found objects appear in the **Custom** section of the device page with checkboxes, like ports.

A table metric gets the labels `index` and `name`; a single value has no labels. Only numeric values are exported. Deleting an OID removes its objects from all devices. All changes are logged.

## Change log

The Change log page shows who added, changed or deleted a device, credentials or a profile and when, changed ticks (which objects were added and removed), settings and passwords, as well as logins (including failed ones) and Refresh runs, scheduled ones too. Entries are kept for a year.

## Users

The Users page (admin only): users with the **admin** role (can change everything) or **read-only** (see devices, ports, settings and the log, but change nothing except their own password). The last administrator cannot be deleted or demoted. The account from `ZIROX_ADMIN_USER` is created on first start; when upgrading from an older version the existing admin is migrated automatically.

## REST API

Tokens are created on Users → API tokens and shown once (the database stores only their SHA-256). A read-only token can only read.

```bash
T=zx_...   # token
H="Authorization: Bearer $T"
curl -H "$H" http://zirox:9116/api/v1/devices                 # all devices with poll status (?group=DC1)
curl -H "$H" http://zirox:9116/api/v1/devices/5               # one device with all found objects
curl -H "$H" -X POST http://zirox:9116/api/v1/devices \
     -d '{"name":"acc-sw-12","address":"10.0.1.12","group":"DC1","profile":"Fast (6 s)","credential":"Nexus RO v3"}'
curl -H "$H" -X PATCH http://zirox:9116/api/v1/devices/5 -d '{"group":"DC2","interval_sec":30,"enabled":false}'
curl -H "$H" -X POST http://zirox:9116/api/v1/devices/5/refresh
curl -H "$H" -X POST http://zirox:9116/api/v1/devices/5/selection \
     -d '{"select":["Ethernet1/5","Supervisor CPU"],"unselect":["Ethernet1/1"]}'
curl -H "$H" -X DELETE http://zirox:9116/api/v1/devices/5
curl -H "$H" http://zirox:9116/api/v1/profiles
curl -H "$H" http://zirox:9116/api/v1/credentials           # names and versions only, no secrets
```

When creating a device you can pass `"version":"2c","community":"..."` instead of `credential`. A new device gets a full walk right away. In `selection`, objects are named by key (port or sensor name, as in the `key` field) or by `id`; objects not named are left as they are. All changes through the API are logged as `api:<token name>`.

## Security

- UI login with username and password (bcrypt), session in a signed cookie (`HttpOnly`, `SameSite=Strict`); cross-site POST requests are rejected.
- Communities and SNMPv3 passwords are stored encrypted in the database (AES-256-GCM) and never shown in the UI.
- SNMPv3 authPriv is supported (SHA/SHA-2, AES/AES-192/AES-256, DES).

## Development

```bash
go test ./...
# Integration test against a real snmpd (v1, v2c, v3), see .github/workflows/ci.yml:
ZIROX_TEST_SNMP=127.0.0.1:1161 go test ./internal/snmpx/
```

Layout: `cmd/zirox` entry point; `internal/snmpx` SNMP, discovery, polling, OID definitions; `internal/engine` scheduling and cache; `internal/exporter` `/probe`, `/sd`, `/metrics`; `internal/web` web UI; `internal/store` SQLite; `internal/secret` encryption.
