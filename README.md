# Zirox SNMP Exporter

An SNMP exporter for Prometheus with a web UI. You add devices in the browser. When a device is added, it is walked **once in full** to find its ports, CPU, memory, temperature sensors, power supplies and fans. After that only the **ticked** objects are polled on schedule, with targeted SNMP GETs and no walks. Prometheus reads the values from the exporter's memory and never talks to the devices.

**Quick start:** see [Install](#install), six steps from an empty Linux server to metrics in Prometheus.

## How it works

1. **Add / Refresh.** A full walk of `ifTable`/`ifXTable`, CISCO-PROCESS-MIB, CISCO-MEMORY-POOL-MIB, CISCO-SYSTEM-EXT-MIB (Nexus), ENTITY-SENSOR-MIB, CISCO-ENVMON-MIB, HOST-RESOURCES-MIB, UCD-SNMP-MIB, CISCO-FIREWALL-MIB and CISCO-REMOTE-ACCESS-MONITOR-MIB (ASA), and BGP4-MIB.
2. **Select.** The device page lists every object with a checkbox. By default physical ports that are up and all health objects are ticked. Ports that are down and VLAN interfaces are not.
3. **Poll.** Only ticked objects, in batches of up to 40 OIDs per GET (three ports per request). For a 48-port switch with 10 ticked ports that is 4 small requests per interval. Every poll also reads `sysUpTime` and the `ifName`/`ifAlias` of the ticked ports, so Zirox can check that the values still belong to the right port (see "Data you can trust" below).
4. **Refresh.** A new full walk, on demand or on schedule (Settings → Scheduled refresh, e.g. weekly at night; off by default). New ports are highlighted and ticked right away if they are up (configurable). Missing ones are marked and no longer polled. Your ticks are kept: ports are matched by name (`ifName`), not by `ifIndex`.

### Data you can trust

- **Port renumbering.** After a reload or a module swap a device may give a port another `ifIndex`. Each poll compares the `ifName` at the polled index with the name found at discovery. If they differ, that port's values are withheld (no traffic is written under the wrong name) and Zirox runs a Refresh by itself, at most once per 15 minutes per device.
- **Restarts.** When `sysUpTime` goes down, the device restarted: Zirox notes it in the change log, exports `zirox_device_last_restart_timestamp_seconds` and refreshes the device.
- **Lost packets.** Requests are independent: if one of them gets no answer, only its objects are affected, the poll is marked partial (`zirox_poll_partial`) and their previous values are repeated once so graphs keep no hole.
- **Port descriptions.** The `ifAlias` label on counters stays as discovered so a changed description does not split the graph; the live description is in `zirox_if_info`.
- **Clear errors.** Failures say what happened and what to check: no answer (ACL, with the address Zirox sends from), access refused (`authorizationError`, community view, `@` in the community), unknown v3 user, wrong v3 password, no route (Docker network), and so on.
- **Database upgrades.** The schema is versioned; before an upgrade changes it, and whenever a new Zirox version starts, a copy of the database is written to `data/backups/` (the newest 5 are kept).

### Web UI

- **Overview** (start page): how many devices poll fine, which do not answer or were never discovered, polls that do not fit their interval, and a "Needs attention" list (not answering, port renumbering, lost packets, failed Refresh, restarts in the last 24 hours, new ports to review). Recent changes are listed next to it.
- **Search** (top left, or press `/`): devices by name, IP, sysName, model or serial, and ports or sensors by name and description. An exact device name or IP opens the device.
- **Device page:** availability for the last 24 hours in half-hour blocks; tabs for Ports, Health and Other; switches with several modules show one block per module with its own "select all". Ticked ports show live In/Out in Mbit/s, load in percent of the port speed and errors per minute, refreshed once per poll interval. The Apply button shows how many ticks you are about to add or remove.
- **Device list:** a 24-hour availability strip per device, bulk Check access / Refresh / Enable / Disable / Delete, and **Change…** to set the group, poll profile or credentials of all selected devices at once.
- **Theme:** auto (follows the system), light or dark; **Compact tables** fits more rows on screen. Both are remembered in the browser.

### Port roles, rules and fast uplinks

Each port can have a **role**: `uplink`, `core`, `server`, `access` or any short word. Set it in the Role column of the device page (or for all visible ports at once), or let **Port rules** do it: a rule matches a regular expression on the port name or description, for all devices or one group, and ticks or unticks the port and sets its role. Rules apply to ports a discovery finds for the first time, top to bottom, first match wins; hand-made ticks and roles stay. **Apply to existing ports** runs them over every known port once. The rules page shows how many ports each rule matches today, with examples.

**Neighbours (CDP and LLDP).** Discovery reads the CDP cache (CISCO-CDP-MIB) and the LLDP remote table (LLDP-MIB) and shows on each port the device on the other end, linked to its page when that device is in Zirox too. A port whose neighbour is another device in Zirox becomes an **uplink** by itself: ticked, role `uplink`, polled fast. This built-in rule comes after your own rules (Settings → "Ports whose CDP/LLDP neighbour is another device in Zirox become uplinks" turns it off). It also applies to known ports once, when a discovery first sees the neighbour there, so after adding the second of two connected switches, press Refresh on the first one (or wait for the scheduled refresh). A role you clear by hand stays cleared. Names are matched without domain and without the "(serial)" that NX-OS adds to its CDP name. Rules can also match the neighbour name, e.g. `^esx-` → role server. Neighbours are exported as `zirox_if_neighbor_info{ifIndex, ifName, neighbor, neighbor_port, protocol}` = 1 for polled ports.

The role is exported as the `role` label, so Grafana can show `role="uplink"` ports of all sites and alerts can treat an uplink differently from a printer port.

**Uplinks are polled faster.** Ports with the role `uplink` are polled every 10 s (Settings → Uplink interval, minimum 5 s, 0 = off) in their own small request, and HTTP service discovery publishes them as a second target with `poll="fast"` and that scrape interval. The rest of the device keeps its own interval. 10 s is the recommended value: Cisco platforms refresh interface counters every few seconds, so 5 s mostly adds noise, while 10 s already shows bursts that a 60 s poll averages away.

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

1. Click **Check access**. Zirox climbs a five-step ladder and stops at the first problem: the name resolves, there is a route (and from which address Zirox sends), the device answers SNMP, the credentials are accepted, the SNMP view shows the interface tables. Each failed step says what to fix and has ready configuration for IOS/IOS-XE, NX-OS, IOS XR and ASA with the Zirox address filled in.
2. Click **Add and discover**. Zirox walks the device once and lists its ports, CPU, memory, sensors, PSUs and fans, and reads the model, software version and serial number (ENTITY-MIB, or `sysDescr` when the device has none).
3. On the device page tick what you want polled and save. Up ports and health objects are ticked by default, down ports are not.

The same check is on every device page (**Diagnostics**) and for many devices at once: select them in the list and click **Check access**. The result lists which devices answer, which do not and why, the addresses to add to the SNMP ACL, and exports a CSV for the network team.

Many devices at once: **Import** takes a CSV (see "CSV import" below), and **Find devices** scans a subnet (up to a /20) with the credentials you choose, lists what answered with its name and model, and adds the ones you tick.

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

Optionally upload the dashboards to Grafana with `deploy/grafana/upload.sh` and add `deploy/alerts.yml` to Prometheus (see "Grafana" and "Alerts").

### Troubleshooting

| Symptom | What to check |
| --- | --- |
| Installer: "port 9116 is already in use" | Another exporter (often the standard snmp_exporter) uses it. Add `--port 9117`, or stop the other one. |
| Installer: "cannot read releases" / download fails | No internet from the server. Use the proxy form of the command from step 2. |
| Web UI does not open from your PC, but `curl http://127.0.0.1:9116/healthz` on the server answers `ok` | A host firewall blocks the port: `sudo firewall-cmd --add-port=9116/tcp --permanent && sudo firewall-cmd --reload` (or open it in `ufw`/iptables). |
| Device shows "The device refused access (authorizationError)" | The community or v3 user is accepted but its SNMP view hides the OIDs. On Cisco a community with `@` is read as `community@context`: use one without `@`. |
| Test connection: "No answer from the device" | Check from the server itself: `snmpget -v2c -c <community> <device-ip> 1.3.6.1.2.1.1.5.0` (package `net-snmp-utils` or `snmp`). If it times out too, the problem is outside Zirox: UDP 161 blocked on the way, the device's SNMP ACL does not allow the server's IP, a wrong community, or SNMP not enabled on the device. |
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
| `ZIROX_UPDATE_CHECK` | on | `0` = do not check GitHub for a newer release (the UI shows "vX.Y.Z available" when there is one) |
| `HTTPS_PROXY` | — | Proxy for the release check; the installer copies it from `https_proxy` |

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

Ports (same names as the standard snmp_exporter, so existing Grafana dashboards work): `ifHCInOctets`, `ifHCOutOctets`, `ifHCInUcastPkts`, `ifHCOutUcastPkts`, `ifInErrors`, `ifOutErrors`, `ifInDiscards`, `ifOutDiscards`, `ifOperStatus`, `ifAdminStatus`, `ifHighSpeed` with the labels `ifIndex`, `ifName`, `ifDescr`, `ifAlias` and `role` (empty unless set; Prometheus ignores empty labels, so series of ports without a role are unchanged). For SNMPv1, `ifInOctets`/`ifOutOctets`/`ifSpeed` replace the 64-bit counters.

Device health: `zirox_cpu_utilization_percent{window}`, `zirox_memory_used_bytes`, `zirox_memory_free_bytes`, `zirox_memory_utilization_percent`, `zirox_memory_total_bytes`, `zirox_memory_available_bytes`, `zirox_temperature_celsius`, `zirox_sensor_value{unit}`, `zirox_sensor_status`, `zirox_envmon_{temperature,fan,psu}_state`, `zirox_storage_{size,used}_bytes`.

Firewalls (ASA): `zirox_firewall_connections{type="current"|"max"}`, `zirox_vpn_sessions{type="all"|"l2l"|"anyconnect"}`, `zirox_vpn_users`.

Servers and IOS-XR: Dell iDRAC (IDRAC-MIB) gives `zirox_temperature_celsius`, `zirox_fan_rpm` and `zirox_dell_status{component="system"|"temperature"|"fan"|"psu"|"disk"}` (3 = ok); HPE iLO (CPQHLTH-MIB) gives `zirox_temperature_celsius` and `zirox_hp_condition{component=...}` (2 = ok); Cisco IOS-XR / NCS memory comes from CISCO-ENHANCED-MEMPOOL-MIB as `zirox_memory_used_bytes` / `zirox_memory_free_bytes`. They are found by the normal discovery; nothing to configure.

BGP peers (BGP4-MIB, IPv4): `zirox_bgp_peer_state` (6 = established), `zirox_bgp_peer_admin_status`, `zirox_bgp_peer_established_seconds`, `zirox_bgp_peer_updates_received_total` with the labels `peer`, `remote_as`.

Port descriptions: `zirox_if_info{ifIndex, ifName, ifDescr, ifAlias, role}` = 1, with the live `ifAlias` read at every poll; `zirox_if_neighbor_info` (see Neighbours). Join it to traffic for the current description: `rate(ifHCInOctets[5m]) * on(device, ifIndex) group_left(ifAlias) zirox_if_info`.

Internal: `zirox_poll_success`, `zirox_poll_duration_seconds`, `zirox_poll_last_success_timestamp_seconds`, `zirox_poll_interval_seconds`, `zirox_poll_samples`, `zirox_poll_partial`, `zirox_poll_requests_failed`, `zirox_ports_index_shifted`, `zirox_device_uptime_seconds`, `zirox_device_last_restart_timestamp_seconds`, `zirox_device_last_discovery_timestamp_seconds`, `zirox_fast_poll_success`, `zirox_fast_poll_duration_seconds`, `zirox_fast_poll_interval_seconds` (uplink target), `zirox_device_info` (labels `sysName`, `sysDescr`, `sysObjectID`, `model`, `version`, `serial`).

Example queries:

```promql
rate(ifHCInOctets{device="core-nx-01"}[5m]) * 8   # inbound traffic, bit/s
increase(ifInErrors[1h]) > 0                        # ports with errors in the last hour
zirox_poll_success == 0                             # devices that do not answer
```

### Alerts

`deploy/alerts.yml` has ready Prometheus rules: device not answering for 5 minutes, CPU > 90 %, memory > 90 %, temperature > 70 °C, PSU/fan/sensor problem (CISCO-ENVMON), Dell iDRAC or HPE iLO component not ok, disk > 90 % full, **uplink down** (critical, after 1 minute, for ports with the role `uplink`), other port down while admin up, port flapping (more than 3 changes in 10 minutes), port errors, port utilization > 90 %, BGP peer not established, device restarted, SNMP packets lost in most polls, poll taking over 80 % of its interval, ports renumbered and not fixed by the automatic Refresh. Adjust the thresholds to your needs.

```yaml
rule_files:
  - /etc/prometheus/zirox-alerts.yml
```

Check: `promtool check rules deploy/alerts.yml && promtool test rules deploy/alerts_test.yml`.

**Notifications through Grafana.** Once Prometheus loads the rules, Grafana shows them under Alerting → Alert rules as data source-managed rules. To send them to Telegram, e-mail or Teams through Grafana contact points, convert them in Grafana 12: Alerting → Alert rules → More → Import to Grafana-managed rules, pick the Prometheus data source and the `zirox-*` groups. Import again after upgrading Zirox to pick up changed rules. Without Grafana, Prometheus sends them to an Alertmanager as usual.

## Grafana

Four linked dashboards in `deploy/grafana/`, all in a "Zirox" folder:

| Dashboard | Shows |
| --- | --- |
| **Zirox NOC** (`zirox-noc`) | Every device of the chosen groups: counters, a device table, top ports by traffic and load, ports down. Click a device or a port to drill down. |
| **Zirox device** (`zirox-device`) | One device: status, gauges, traffic, 95th percentile, all ports, health (CPU, memory, temperature, disks, Cisco/Dell/HPE hardware state), firewall and BGP. Click a port to open it. |
| **Zirox port** (`zirox-port`) | One port: status, speed, traffic (download above, upload below the axis), load, peak and 95th percentile over the range, packets, errors, link flaps. |
| **Zirox health** | Zirox itself: devices failing, partial and too slow polls, uplink polls, the slowest devices, memory, series in Prometheus. |

The device and port dashboards mark **device restarts** (red) and **Zirox refreshes** (blue) on the time axis, so a traffic drop next to a reboot or a re-discovery explains itself.

Upload or update them with one command (a Grafana service account token with the Editor role; Administration → Users and access → Service accounts):

```sh
curl -fsSL https://raw.githubusercontent.com/Boor0n/-zirox-snmp-exporter/main/deploy/grafana/upload.sh |
  GRAFANA_URL=http://127.0.0.1:3000 GRAFANA_TOKEN=glsa_... sh
```

Or import the four JSON files by hand: Dashboards → New → Import. Pick the data source in the selector at the top of each dashboard; Prometheus and VictoriaMetrics both work.

The panels come from `deploy/grafana-dashboard.json`, the older single "Zirox SNMP" dashboard with the same content in sections, which keeps working on its own. After changing it, run `python3 deploy/grafana/build.py`; CI checks that the generated files are current and that every query parses.

Traffic is shown in Mbit/s and switches to Gbit/s automatically. Rates use a 5 minute window, so graphs work with any poll interval up to 2 minutes.

Since the port metric names match the standard snmp_exporter, ready dashboards from the Grafana catalog work too.

## Long history: VictoriaMetrics

Prometheus keeps 15 days by default and needs a lot of memory for years of data. VictoriaMetrics stores the same series in several times less disk and memory and answers PromQL, so the dashboards and alerts stay the same. Keep Prometheus for scraping and alerts and let it copy every sample to VictoriaMetrics:

```sh
docker run -d --name victoriametrics --restart unless-stopped -p 8428:8428 \
  -v /var/lib/victoria-metrics:/victoria-metrics-data \
  victoriametrics/victoria-metrics:v1.102.1 -retentionPeriod=5y
```

```yaml
# prometheus.yml
remote_write:
  - url: http://victoriametrics-host:8428/api/v1/write
```

Then add a Prometheus-type data source in Grafana with the URL `http://victoriametrics-host:8428` and pick it in the dashboards for long ranges. Rough disk size: 300 devices with 50 ticked ports each give about 165,000 series; at a 60 s interval that is about 40-50 GB per year in VictoriaMetrics (uplinks at 10 s add six times as much per uplink port). Prometheus can then keep a short local retention (`--storage.tsdb.retention.time=15d`).

## CSV import

Import takes a CSV with a header. Only the `address` column is required. Optional: `name`, `port`, `group`, `profile` (profile name), `credential` (name of saved credentials) or `version` + `community` for v1/v2c.

```csv
name,address,group,profile,credential
core-nx-01,10.0.0.1,DC1,Fast (6 s),Nexus RO v3
acc-sw-12,10.0.1.12,DC1,,
```

After the import, **Check access of the new devices** runs the access check for all of them.

## Custom OIDs

If an OID is not among the built-in ones (ports, CPU, memory, sensors, disks, firewall, VPN, BGP), add it on the **Custom OIDs** page:

1. In "Check an OID on a device", pick a device and an OID. Turn on **Walk** to see a whole table or subtree. You see right away what the device returns and which values are numeric.
2. Click **Add column** to poll a table column (one value per row: per port, sensor, pool), or **Add value** for a single value (usually an OID ending in `.0`).
3. Set the metric name (e.g. `cisco_mempool_used_bytes`), the type (gauge or counter) and, if needed, a multiplier. For a table you can name a column with row names; it becomes the `name` label.
4. Click **Refresh all devices** or Refresh on a device. Found objects appear in the **Custom** section of the device page with checkboxes, like ports.

A table metric gets the labels `index` and `name`; a single value has no labels. Only numeric values are exported. Deleting an OID removes its objects from all devices. All changes are logged.

## Backup and configuration

**System → Backup and config** (admins):

- **Download backup**: the database and the encryption key as one `.tar.gz` with a `RESTORE.txt`. Restoring it on a new server (stop Zirox, extract into `/var/lib/zirox`, start) moves everything: devices, ticks, credentials, users, change log. If the key is set by `ZIROX_SECRET_KEY`, it is not in the archive; set the same value on the new server.
- **Export configuration**: settings, credentials (without secrets unless you ask), poll profiles, port rules, custom OIDs and devices with their ticked ports and roles, as YAML you can keep in git.
- **Import configuration**: upload or paste such a file, press **Preview** to see every change and any problem, then **Import**. Devices match by name or address; ticks and roles are restored on known devices, and new devices get them after their first discovery. Port rules in the file replace the current ones. The database is backed up before the import.

## Change log

The Change log page shows who added, changed or deleted a device, credentials or a profile and when, changed ticks (which objects were added and removed), settings and passwords, as well as logins (including failed ones) and Refresh runs, scheduled ones too. Entries are kept for a year.

## Users

The Users page (admin only): users with the **admin** role (can change everything) or **read-only** (see devices, ports, settings and the log, but change nothing except their own password). The last administrator cannot be deleted or demoted. The account from `ZIROX_ADMIN_USER` is created on first start; when upgrading from an older version the existing admin is migrated automatically.

## REST API

Tokens are created on Users → API tokens and shown once (the database stores only their SHA-256). A read-only token can only read. Tokens expire after the period chosen at creation (a year by default).

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
- Login throttling: after 5 wrong passwords in 15 minutes from one address, or for one user name, logins from that address and for that name are refused for 15 minutes (also with the right password), and the change log records "login blocked". Sessions last 12 hours; changing a user's password or role ends that user's sessions.
- Every page is sent with a strict Content-Security-Policy (scripts only from Zirox itself, no framing), `X-Content-Type-Options: nosniff` and `Referrer-Policy: same-origin`.
- API tokens expire after 30 days, 90 days or a year (chosen when created; "never" is still possible). An expired token gets `401` with the date it expired.
- Communities and SNMPv3 passwords are stored encrypted in the database (AES-256-GCM) and never shown in the UI.
- SNMPv3 authPriv is supported (SHA/SHA-2, AES/AES-192/AES-256, DES).

## Development

```bash
go test ./...
# Integration test against a real snmpd (v1, v2c, v3), see .github/workflows/ci.yml:
ZIROX_TEST_SNMP=127.0.0.1:1161 go test ./internal/snmpx/
```

Layout: `cmd/zirox` entry point; `internal/snmpx` SNMP, discovery, polling, OID definitions; `internal/engine` scheduling and cache; `internal/exporter` `/probe`, `/sd`, `/metrics`; `internal/web` web UI; `internal/store` SQLite; `internal/secret` encryption.
