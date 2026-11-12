#!/usr/bin/env python3
"""Builds the Zirox Grafana dashboards from deploy/grafana-dashboard.json.

    python3 deploy/grafana/build.py

writes four linked dashboards next to this script:

  zirox-noc.json     NOC overview: every device of the chosen groups.
  zirox-device.json  One device: ports, health, firewall and routing.
  zirox-port.json    One port: traffic, load, errors, state history.
  zirox-health.json  Zirox itself: poll success, durations, scrape load.

A device name in the NOC opens the device dashboard, a port in the device
dashboard opens the port dashboard. The device and port dashboards show
device restarts and Zirox refreshes as annotations.

The single dashboard deploy/grafana-dashboard.json stays the source of the
shared panels, and keeps working on its own. Edit panels there (or here for
the port and health dashboards) and run this script again.
"""
import copy
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "..", "grafana-dashboard.json")
DS = {"type": "prometheus", "uid": "${datasource}"}


def load():
    with open(SRC) as f:
        return json.load(f)


def save(path, d):
    with open(path, "w") as f:
        json.dump(d, f, indent=2, ensure_ascii=False)
        f.write("\n")


def walk(panels):
    for p in panels:
        yield p
        yield from walk(p.get("panels", []))


def hardware_fix(src):
    """Counts Dell iDRAC and HPE iLO problems with the Cisco sensors."""
    for p in walk(src["panels"]):
        if p.get("title") == "Hardware problems":
            p["targets"][0]["expr"] = (
                '(count({__name__=~"zirox_envmon_.+_state", group=~"$group"} > 1 != 5) or vector(0))'
                ' + (count(zirox_dell_status{group=~"$group"} > 3) or vector(0))'
                ' + (count(zirox_hp_condition{group=~"$group"} > 2) or vector(0))'
            )
            p["description"] = ("Cisco sensors not normal, Dell iDRAC components worse than ok, "
                                "HPE iLO components degraded or failed.")
        if p.get("title", "").startswith("Hardware state"):
            p["title"] = "Hardware state (fans, power supplies, sensors, disks)"
            p["targets"] = [
                {"datasource": DS, "expr": '{__name__=~"zirox_envmon_.+_state", device="$device"}',
                 "format": "table", "instant": True, "range": False, "refId": "A"},
                {"datasource": DS, "expr": 'zirox_dell_status{device="$device"}',
                 "format": "table", "instant": True, "range": False, "refId": "B"},
                {"datasource": DS, "expr": 'zirox_hp_condition{device="$device"}',
                 "format": "table", "instant": True, "range": False, "refId": "C"},
            ]
            p["transformations"] = [
                {"id": "merge", "options": {}},
                {"id": "organize", "options": {
                    "excludeByName": {"Time": True, "job": True, "instance": True, "address": True,
                                      "sysName": True, "group": True, "device": True, "index": True, "poll": True},
                    "renameByName": {"__name__": "metric", "Value #A": "Cisco state",
                                     "Value #B": "Dell status", "Value #C": "HPE condition"}}},
            ]
            p["description"] = ("Cisco: 1=normal, 2=warning, 3=critical, 4=shutdown, 5=notPresent, 6=notFunctioning. "
                                "Dell: 3=ok, 4=nonCritical, 5=critical, 6=nonRecoverable. "
                                "HPE: 2=ok, 3=degraded, 4=failed.")


def hide_poll(src):
    """Uplinks polled fast carry poll="fast"; tables need not show it."""
    for p in walk(src["panels"]):
        for tr in p.get("transformations", []):
            if tr["id"] == "organize":
                tr["options"].setdefault("excludeByName", {})["poll"] = True


def var(src, name):
    for v in src["templating"]["list"]:
        if v["name"] == name:
            return copy.deepcopy(v)
    raise KeyError(name)


def query_var(name, label, q, multi=False, include_all=False, all_value=None):
    v = {"type": "query", "name": name, "label": label, "datasource": DS, "definition": q,
         "query": {"query": q, "refId": "PrometheusVariableQueryEditor-VariableQuery", "qryType": 1},
         "refresh": 2, "multi": multi, "includeAll": include_all, "current": {}, "sort": 1, "options": []}
    if all_value:
        v["allValue"] = all_value
    return v


def annotations(device_expr):
    """Device restarts and Zirox refreshes as markers on the time axis."""
    base = {"datasource": DS, "enable": True, "step": "60s", "useValueForTime": False}
    return {"list": [
        {"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"}, "enable": True,
         "hide": True, "iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"},
        dict(base, name="Device restart", iconColor="red", titleFormat="{{device}} restarted",
             textFormat="sysUpTime went back: the device rebooted.",
             expr=f"changes(zirox_device_last_restart_timestamp_seconds{{{device_expr}}}[2m]) > 0"),
        dict(base, name="Zirox refresh", iconColor="blue", titleFormat="{{device}} refreshed",
             textFormat="Discovery ran (Add or Refresh in Zirox): ports may have been ticked or unticked.",
             expr=f"changes(zirox_device_last_discovery_timestamp_seconds{{{device_expr}}}[2m]) > 0"),
    ]}


def dashboard(src, uid, title, desc, variables, panels, ann=None):
    d = {k: copy.deepcopy(src[k]) for k in (
        "__requires", "timezone", "editable", "graphTooltip", "refresh", "schemaVersion",
        "liveNow", "fiscalYearStartMonth", "time", "timepicker")}
    d.update({
        "uid": uid, "title": title, "description": desc, "version": 1,
        "tags": ["zirox", "snmp"],
        "templating": {"list": [var(src, "datasource")] + variables},
        "annotations": ann or annotations('device="__none__"'),
        "links": [{"type": "dashboards", "tags": ["zirox"], "asDropdown": True, "title": "Zirox",
                   "includeVars": False, "keepTime": True}],
        "panels": panels,
    })
    if not ann:
        d["annotations"]["list"] = d["annotations"]["list"][:1]
    return d


def section(src, first_row, last_row_exclusive=None):
    """Panels from the row titled first_row up to the next listed row."""
    ps = src["panels"]
    start = next(i for i, p in enumerate(ps) if p["type"] == "row" and p["title"].startswith(first_row))
    end = len(ps)
    if last_row_exclusive:
        end = next(i for i, p in enumerate(ps) if p["type"] == "row" and p["title"].startswith(last_row_exclusive))
    return copy.deepcopy(ps[start:end])


def shift(panels):
    """Moves panels up so the first starts at y=0."""
    y0 = min(p["gridPos"]["y"] for p in panels)
    for p in walk(panels):
        p["gridPos"]["y"] -= y0
    return panels


def relink(panels, column, url, title):
    for p in walk(panels):
        for o in p.get("fieldConfig", {}).get("overrides", []):
            if o["matcher"].get("options") == column:
                for prop in o["properties"]:
                    if prop["id"] == "links":
                        prop["value"] = [{"title": title, "targetBlank": False, "url": url}]


def add_link(panel, column, url, title):
    ovs = panel.setdefault("fieldConfig", {}).setdefault("overrides", [])
    for o in ovs:
        if o["matcher"].get("options") == column:
            o["properties"] = [p for p in o["properties"] if p["id"] != "links"]
            o["properties"].append({"id": "links", "value": [{"title": title, "targetBlank": False, "url": url}]})
            return
    ovs.append({"matcher": {"id": "byName", "options": column},
                "properties": [{"id": "links", "value": [{"title": title, "targetBlank": False, "url": url}]}]})


DEVICE_URL = "/d/zirox-device/zirox-device?${datasource:queryparam}&var-device=${__value.raw}&${__url_time_range}"
DEVICE_URL_ROW = "/d/zirox-device/zirox-device?${datasource:queryparam}&var-device=${__data.fields.Device}&${__url_time_range}"
PORT_URL = ("/d/zirox-port/zirox-port?${datasource:queryparam}&var-device=${device}"
            "&var-port=${__value.raw}&${__url_time_range}")
PORT_URL_ROW = ("/d/zirox-port/zirox-port?${datasource:queryparam}&var-device=${__data.fields.Device}"
                "&var-port=${__data.fields.Port}&${__url_time_range}")


def t(expr, legend="", ref="A", instant=False, fmt=None):
    x = {"datasource": DS, "expr": expr, "legendFormat": legend, "refId": ref,
         "range": not instant, "instant": instant}
    if fmt:
        x["format"] = fmt
    return x


def ts(pid, title, x, y, w, h, targets, unit="none", desc="", stack=False, extra=None):
    p = {"type": "timeseries", "title": title, "description": desc, "id": pid, "datasource": DS,
         "gridPos": {"h": h, "w": w, "x": x, "y": y},
         "fieldConfig": {"defaults": {"unit": unit, "custom": {
             "fillOpacity": 15, "lineWidth": 1, "showPoints": "never", "lineInterpolation": "smooth",
             "gradientMode": "opacity", "spanNulls": 300000,
             "stacking": {"mode": "normal" if stack else "none"}}}, "overrides": []},
         "options": {"legend": {"showLegend": True, "displayMode": "table", "placement": "bottom",
                                "calcs": ["mean", "max", "lastNotNull"], "sortBy": "Max", "sortDesc": True},
                     "tooltip": {"mode": "multi", "sort": "desc"}},
         "targets": targets}
    if extra:
        p["fieldConfig"]["overrides"] += extra
    return p


def stat(pid, title, x, y, w, h, targets, unit="none", desc="", mappings=None, thresholds=None, decimals=None):
    d = {"unit": unit, "mappings": mappings or [],
         "thresholds": thresholds or {"mode": "absolute", "steps": [{"color": "text", "value": None}]}}
    if decimals is not None:
        d["decimals"] = decimals
    return {"type": "stat", "title": title, "description": desc, "id": pid, "datasource": DS,
            "gridPos": {"h": h, "w": w, "x": x, "y": y},
            "fieldConfig": {"defaults": d, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                        "colorMode": "background" if mappings or thresholds else "none",
                        "graphMode": "none", "textMode": "value", "justifyMode": "center"},
            "targets": targets}


def steps(*pairs):
    return {"mode": "absolute", "steps": [{"color": c, "value": v} for v, c in pairs]}


OPER = [{"type": "value", "options": {
    "1": {"text": "up", "color": "green"}, "2": {"text": "down", "color": "red"},
    "3": {"text": "testing", "color": "orange"}, "5": {"text": "dormant", "color": "text"},
    "6": {"text": "notPresent", "color": "text"}, "7": {"text": "lowerLayerDown", "color": "red"}}}]
ADMIN = [{"type": "value", "options": {
    "1": {"text": "enabled", "color": "green"}, "2": {"text": "shut", "color": "text"},
    "3": {"text": "testing", "color": "orange"}}}]


def noc(src):
    ps = shift([p for p in section(src, "Overview", "Device") if p["type"] != "row"])
    relink(ps, "Device", DEVICE_URL, "Open ${__value.raw}")
    for p in ps:
        if p["type"] == "table" and p["title"] != "Devices":
            add_link(p, "Port", PORT_URL_ROW, "Open port ${__value.raw}")
            add_link(p, "Device", DEVICE_URL, "Open ${__value.raw}")
    return dashboard(
        src, "zirox-noc", "Zirox NOC",
        "Every device of the chosen groups: who is down, what is busy. Click a device or a port to drill down.",
        [var(src, "group")], ps)


def device(src):
    ps = shift(section(src, "Device"))
    for p in walk(ps):
        if p["type"] == "table" and p["title"] == "Ports":
            add_link(p, "Port", PORT_URL, "Open port ${__value.raw}")
    dv = query_var("device", "Device", "label_values(zirox_poll_success{group=~\"$group\"}, device)")
    return dashboard(
        src, "zirox-device", "Zirox device",
        "One device: status, ports, health, firewall and routing. Click a port in the Ports table to open it.",
        [var(src, "group"), dv, var(src, "interface")], ps,
        annotations('device="$device"'))


def port(src):
    sel = 'device="$device", ifName="$port"'
    pv = query_var("port", "Port", 'label_values(ifOperStatus{device="$device"}, ifName)')
    dv = query_var("device", "Device", "label_values(zirox_poll_success, device)")
    mbit = lambda m: f"rate({m}{{{sel}}}[2m]) * 8 / 1e6"
    traffic_over = [
        {"matcher": {"id": "byRegexp", "options": "/^In/"},
         "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": "#56C26A"}}]},
        {"matcher": {"id": "byRegexp", "options": "/^Out/"},
         "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": "#2BC4C4"}},
                        {"id": "custom.transform", "value": "negative-Y"}]},
    ]
    load = (f'100 * clamp_min(rate(ifHCInOctets{{{sel}}}[2m]), 0) * 8 / (ifHighSpeed{{{sel}}} * 1e6 > 0)')
    load_out = load.replace("ifHCInOctets", "ifHCOutOctets")
    panels = [
        {"type": "row", "title": "$device $port", "id": 1, "collapsed": False,
         "gridPos": {"h": 1, "w": 24, "x": 0, "y": 0}, "panels": []},
        stat(2, "Status", 0, 1, 3, 4, [t(f"ifOperStatus{{{sel}}}", instant=True)], mappings=OPER),
        stat(3, "Admin", 3, 1, 3, 4, [t(f"ifAdminStatus{{{sel}}}", instant=True)], mappings=ADMIN),
        stat(4, "Speed", 6, 1, 3, 4, [t(f"ifHighSpeed{{{sel}}}", instant=True)], unit="Mbits"),
        stat(5, "In now", 9, 1, 3, 4, [t(mbit("ifHCInOctets"), instant=True)], unit="Mbits", decimals=1),
        stat(6, "Out now", 12, 1, 3, 4, [t(mbit("ifHCOutOctets"), instant=True)], unit="Mbits", decimals=1),
        stat(7, "Peak load", 15, 1, 3, 4,
             [t(f"max_over_time(({load})[$__range:1m])", instant=True)], unit="percent", decimals=0,
             thresholds=steps((None, "green"), (70, "orange"), (90, "red")),
             desc="Highest load in or out over the shown time range."),
        stat(8, "95th percentile in", 18, 1, 3, 4,
             [t(f"quantile_over_time(0.95, ({mbit('ifHCInOctets')})[$__range:1m])", instant=True)],
             unit="Mbits", decimals=1, desc="95th percentile of 1-minute traffic over the shown range."),
        stat(9, "Errors in range", 21, 1, 3, 4,
             [t(f"sum(increase(ifInErrors{{{sel}}}[$__range]) + increase(ifOutErrors{{{sel}}}[$__range]))",
                instant=True)], decimals=0, thresholds=steps((None, "green"), (1, "red"))),
        ts(10, "Traffic", 0, 5, 24, 10,
           [t(mbit("ifHCInOctets"), "In"), t(mbit("ifHCOutOctets"), "Out", "B")], unit="Mbits",
           desc="Download above the axis, upload below. Uplinks polled every few seconds show bursts here.",
           extra=traffic_over),
        ts(11, "Load", 0, 15, 12, 8, [t(load, "In"), t(load_out, "Out", "B")], unit="percent",
           desc="Traffic as a share of the port speed."),
        ts(12, "Packets per second", 12, 15, 12, 8,
           [t(f"rate(ifHCInUcastPkts{{{sel}}}[2m])", "In"),
            t(f"rate(ifHCOutUcastPkts{{{sel}}}[2m])", "Out", "B")], unit="pps"),
        ts(13, "Errors and discards per minute", 0, 23, 12, 8, [
            t(f"60 * rate(ifInErrors{{{sel}}}[5m])", "In errors"),
            t(f"60 * rate(ifOutErrors{{{sel}}}[5m])", "Out errors", "B"),
            t(f"60 * rate(ifInDiscards{{{sel}}}[5m])", "In discards", "C"),
            t(f"60 * rate(ifOutDiscards{{{sel}}}[5m])", "Out discards", "D")]),
        {"type": "state-timeline", "title": "Link state", "description": "Operational status over time: flaps show as short red bars.",
         "id": 14, "datasource": DS, "gridPos": {"h": 8, "w": 12, "x": 12, "y": 23},
         "fieldConfig": {"defaults": {"mappings": OPER, "custom": {"fillOpacity": 80, "lineWidth": 0},
                                      "thresholds": steps((None, "text"))}, "overrides": []},
         "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8,
                     "legend": {"showLegend": False}, "tooltip": {"mode": "single"}},
         "targets": [t(f"ifOperStatus{{{sel}}}", "Link")]},
        {"type": "table", "title": "Port details", "id": 15, "datasource": DS,
         "gridPos": {"h": 5, "w": 24, "x": 0, "y": 31},
         "description": "Labels Zirox exports for this port: description, role, index, and the CDP/LLDP neighbour.",
         "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}}, "overrides": []},
         "options": {"showHeader": True},
         "targets": [t(f"zirox_if_info{{{sel}}}", instant=True, fmt="table"),
                     t(f"zirox_if_neighbor_info{{{sel}}}", ref="B", instant=True, fmt="table")],
         "transformations": [{"id": "merge", "options": {}}, {"id": "organize", "options": {
             "excludeByName": {"Time": True, "Value #A": True, "Value #B": True, "__name__": True, "job": True,
                               "instance": True, "poll": True}}}]},
    ]
    return dashboard(
        src, "zirox-port", "Zirox port",
        "One port: traffic, load, packets, errors and link flaps, with device restarts and Zirox refreshes marked.",
        [dv, pv], panels, annotations('device="$device"'))


def health(src):
    panels = [
        stat(1, "Devices polled", 0, 0, 4, 4, [t("count(zirox_poll_success)", instant=True)]),
        stat(2, "Failing", 4, 0, 4, 4, [t("count(zirox_poll_success == 0) or vector(0)", instant=True)],
             thresholds=steps((None, "green"), (1, "red"))),
        stat(3, "Partial polls", 8, 0, 4, 4, [t("count(zirox_poll_partial > 0) or vector(0)", instant=True)],
             thresholds=steps((None, "green"), (1, "orange")),
             desc="Devices where some SNMP requests failed but the rest arrived."),
        stat(4, "Too slow", 12, 0, 4, 4,
             [t("count(zirox_poll_duration_seconds > 0.8 * zirox_poll_interval_seconds) or vector(0)", instant=True)],
             thresholds=steps((None, "green"), (1, "orange")),
             desc="Devices whose poll takes over 80% of their interval: tick fewer ports or raise the interval."),
        stat(5, "Uplink polls failing", 16, 0, 4, 4,
             [t("count(zirox_fast_poll_success == 0) or vector(0)", instant=True)],
             thresholds=steps((None, "green"), (1, "red"))),
        stat(6, "Samples per scrape", 20, 0, 4, 4, [t("sum(zirox_poll_samples)", instant=True)],
             decimals=0, desc="Values Zirox hands Prometheus each round: grows with ticked ports."),
        ts(7, "Devices by state", 0, 4, 12, 8, [t("zirox_devices", "{{state}}")], stack=True,
           desc="From the Zirox job (/metrics)."),
        ts(8, "Slowest polls", 12, 4, 12, 8,
           [t("topk(10, zirox_poll_duration_seconds)", "{{device}}"),
            t("topk(5, zirox_fast_poll_duration_seconds)", "{{device}} uplinks", "B")], unit="s"),
        ts(9, "Poll success", 0, 12, 12, 8,
           [t("sum(zirox_poll_success)", "ok"), t("count(zirox_poll_success == 0)", "failing", "B")]),
        ts(10, "Failed SNMP requests", 12, 12, 12, 8,
           [t("topk(10, zirox_poll_requests_failed > 0)", "{{device}}")]),
        ts(11, "Zirox memory", 0, 20, 8, 8,
           [t('process_resident_memory_bytes{job=~".*zirox.*", device=""}', "{{instance}}")], unit="bytes",
           desc="Needs the job that scrapes Zirox /metrics."),
        ts(12, "Scrape time", 8, 20, 8, 8,
           [t("topk(10, scrape_duration_seconds{device!=\"\"})", "{{device}} {{poll}}")], unit="s"),
        ts(13, "Prometheus series", 16, 20, 8, 8,
           [t("prometheus_tsdb_head_series", "{{instance}}"), t("vm_cache_entries{type=\"storage/hour_metric_ids\"}", "VictoriaMetrics", "B")],
           desc="Active series in the time-series database (needs it to scrape itself)."),
        {"type": "table", "title": "Devices not responding", "id": 14, "datasource": DS,
         "gridPos": {"h": 8, "w": 24, "x": 0, "y": 28},
         "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}}, "overrides": [
             {"matcher": {"id": "byName", "options": "Last success"},
              "properties": [{"id": "unit", "value": "dateTimeFromNow"}]}]},
         "options": {"showHeader": True},
         "targets": [t("zirox_poll_last_success_timestamp_seconds * 1000 and on(device) zirox_poll_success == 0",
                       instant=True, fmt="table")],
         "transformations": [{"id": "organize", "options": {
             "excludeByName": {"Time": True, "__name__": True, "job": True, "instance": True},
             "renameByName": {"device": "Device", "group": "Group", "address": "Address", "Value": "Last success"}}}]},
    ]
    add_link(panels[-1], "Device", DEVICE_URL, "Open ${__value.raw}")
    return dashboard(
        src, "zirox-health", "Zirox health",
        "Zirox itself: are polls succeeding, are they fast enough, how much does Prometheus store.",
        [], panels)


def main():
    src = load()
    hardware_fix(src)
    hide_poll(src)
    save(SRC, src)
    for name, fn in (("zirox-noc", noc), ("zirox-device", device), ("zirox-port", port), ("zirox-health", health)):
        save(os.path.join(HERE, name + ".json"), fn(src))
        print("wrote", name + ".json")


if __name__ == "__main__":
    main()
