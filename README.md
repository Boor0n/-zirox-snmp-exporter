# Zirox SNMP Exporter

An SNMP exporter for Prometheus with a web UI. You add devices in the browser. When a device is added, it is walked **once in full** to find its ports, CPU, memory, temperature sensors, power supplies and fans. After that only the **ticked** objects are polled on schedule, with targeted SNMP GETs and no walks. Prometheus reads the values from the exporter's memory and never talks to the devices.

**Quick start:** see [Install](#install), six steps from an empty Linux server to metrics in Prometheus.

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

Step by step, from an empty Linux server to metrics in Prometheus. Steps 1–4 take about five minutes.

### 1. Prerequisites

- A Linux server (x86-64 or ARM64; tested on RHEL/Rocky/Alma, Ubuntu, Debian) with `curl`, `tar` and root access via `sudo`.
- **Docker** installed and running (`docker info` works). Without Docker the installer sets up a systemd service instead.
- Network access from the server to your devices on **UDP 161** (SNMP), and from your browser and Prometheus to the server on **TCP 9116** (or the port you choose).
- Internet access to `github.com` and `api.github.com`, directly or through an HTTP proxy. No GitHub account or token is needed: the repository is public.

### 2. Run the installer

```bash
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh
```

**Behind a proxy**, pass it to `curl` with `-x` and to the script with `https_proxy` (the script downloads the release itself):

```bash
P=http://proxy.example.com:8080
curl -fsSL -x "$P" https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo https_proxy=$P sh
```

The script downloads the latest release, verifies its SHA256, loads the image into Docker and starts the `zirox` container (`--restart unless-stopped`), waits for the web UI and prints something like:

```
==> Installed v0.4.0 (docker).
    Web UI:     http://server.example.com:9116
    Login:      admin / Xk3...  (saved in /etc/zirox/zirox.env)
    Prometheus: http://server.example.com:9116/sd (HTTP service discovery)
```

Data (database and encryption key) lives in `/var/lib/zirox`, settings in `/etc/zirox/zirox.env`. On SELinux hosts (RHEL, Rocky, Alma) the data directory is labelled for the container automatically.

#### Options

Options go after `sh -s --`, for example:

```bash
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh -s -- --port 9117 --host-network
```

| Option | What it does |
| --- | --- |
| `--port N` | Port of the web UI, `/sd` and `/probe` (default 9116). Use another one if 9116 is taken, e.g. by the standard snmp_exporter; the installer tells you when it is. |
| `--host-network` | Run the container on the host's network, so SNMP leaves exactly as from the host. Use it when the host reaches the devices but discovery in Zirox fails with "no route to host" or timeouts (firewall or routing rules that do not cover Docker's bridge network). `--bridge-network` switches back. The choice is kept on upgrades. |
| `--systemd` | Install the binary as a systemd service instead of Docker (the default when Docker is not running). |
| `--version vX.Y.Z` | Install a specific release instead of the latest, e.g. to roll back. |
| `--uninstall` | Remove the container or the service. Data and settings are kept. |

**Upgrade:** run the same command again. The database, key, settings, port and network mode are kept.

### 3. First login

Open `http://<server>:9116` and log in as `admin` with the password the installer printed. If you missed it:

```bash
sudo grep ADMIN_PASSWORD /etc/zirox/zirox.env
```

This password is used only on the first start; change it later in **Settings**. To reset a forgotten admin password:

```bash
sudo vi /etc/zirox/zirox.env
#   set   ZIROX_ADMIN_PASSWORD=<new password>
#   add   ZIROX_RESET_ADMIN_PASSWORD=1
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/get.sh | sudo sh   # recreates the container with the new settings
```

Log in with the new password, then remove the `ZIROX_RESET_ADMIN_PASSWORD=1` line and run the installer once more, otherwise the password is reset on every restart. With `--systemd`, `sudo systemctl restart zirox` is enough instead of the installer.

### 4. Add SNMP credentials

**Credentials → New**: give it a name (e.g. `Nexus RO v2c`), pick the SNMP version and enter the community (v1/v2c) or the user, auth and privacy settings (v3). Credentials are stored encrypted and reused by many devices, so you enter a community only once.

### 5. Add a device

**Devices → Add device**: enter the IP address or DNS name, a name (it becomes the `device` label in Prometheus), an optional group/site, the credentials and a poll profile.

1. Click **Test connection** first. It reads `sysName` and `sysDescr` and shows the device's answer or the error.
2. Click **Add and discover**. Zirox walks the device once and lists its ports, CPU, memory, sensors, PSUs and fans.
3. On the device page tick what you want polled and save. Up ports and health objects are ticked by default, down ports are not.

Many devices at once: **Import** takes a CSV (see "CSV import" below).

### 6. Connect Prometheus

Add a job to `prometheus.yml` and reload Prometheus:

```yaml
scrape_configs:
  - job_name: zirox-snmp
    http_sd_configs:
      - url: http://<zirox-server>:9116/sd
        refresh_interval: 60s
```

Prometheus gets every device as a separate target, with its own poll interval. New devices appear without editing the config. Check **Status → Targets** in Prometheus: the `zirox-snmp` targets should be UP. The **Prometheus** page in the Zirox UI shows the same config with your server's address filled in. More in the "Prometheus" section below.

Optionally import `deploy/grafana-dashboard.json` into Grafana and add `deploy/alerts.yml` to Prometheus (see "Grafana" and "Alerts").

### Troubleshooting

| Symptom | What to check |
| --- | --- |
| Installer: "port 9116 is already in use" | Another exporter (often the standard snmp_exporter) uses it. Add `--port 9117`, or stop the other one. |
| Installer: "cannot read releases" / download fails | No internet from the server. Use the proxy form of the command from step 2. |
| Web UI does not open from your PC, but `curl http://127.0.0.1:9116/healthz` on the server answers `ok` | A host firewall blocks the port: `sudo firewall-cmd --add-port=9116/tcp --permanent && sudo firewall-cmd --reload` (or open it in `ufw`/iptables). |
| Test connection: timeout | Check from the server itself: `snmpget -v2c -c <community> <device-ip> 1.3.6.1.2.1.1.5.0` (package `net-snmp-utils` or `snmp`). If it times out too, the problem is outside Zirox: UDP 161 blocked on the way, the device's SNMP ACL does not allow the server's IP, a wrong community, or SNMP not enabled on the device. |
| `snmpget` from the host works, Zirox gets "no route to host" or timeouts | Docker's bridge network is not allowed out. Reinstall with `--host-network`. |
| Some ports or sensors are missing | The SNMP view on the device may hide them. Check with `snmpwalk -v2c -c <community> <device-ip> 1.3.6.1.2.1.31.1.1.1.1` (ifName), then click **Refresh** on the device. |
| Prometheus target is DOWN | Prometheus cannot reach the server on the Zirox port, or `ZIROX_SCRAPE_TOKEN` is set and Prometheus does not send it. |
| Anything else | Logs: `sudo docker logs zirox` (Docker) or `sudo journalctl -u zirox` (systemd). |

### Alternative: Docker from source

```bash
docker compose up -d --build
docker logs zirox   # if ZIROX_ADMIN_PASSWORD is empty, the generated admin password is printed here
```

Open `http://<server>:9116`.

### Alternative: release archive without Docker (systemd)

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

`deploy/grafana-dashboard.json` is the "Zirox SNMP" dashboard. Import: Grafana → Dashboards → New → Import → upload the file, pick the Prometheus data source. If a dashboard with the same name already exists, choose "Import (Overwrite)". Filters at the top: group, device (one at a time), port. Sections:

- **Overview** (for the selected groups): number of devices, how many do not answer, ports down, hardware problems, total traffic; a table of all devices with status, CPU, temperature, ports and traffic (click a device name to open it); top 10 ports by traffic and by load; a list of ports that are down;
- **Device**: status, CPU, memory and temperature gauges, ports up/down, traffic in/out; one traffic graph with inbound above zero and outbound below (idle ports are hidden, the legend is sortable by mean/max/last); load in % of port speed; errors and discards per minute; a table of all ports with status, speed, in/out in Mbit/s, load and errors in the last hour;
- **Health**: CPU, memory, temperature, disks, PSU and fan state;
- **Firewall and routing** (collapsed): ASA connections and VPN sessions, a table of BGP peers.

Traffic is shown in Mbit/s and switches to Gbit/s automatically. Rates use a 5 minute window, so graphs work with any poll interval up to 2 minutes.

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
