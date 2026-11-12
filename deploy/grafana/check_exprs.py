#!/usr/bin/env python3
"""Turns every PromQL query of the dashboards into a recording rule, so
`promtool check rules` catches syntax errors. Used by CI:

    python3 deploy/grafana/check_exprs.py . /tmp/dash-rules.yml
    promtool check rules /tmp/dash-rules.yml
"""
import json, glob, re, sys
rules = []
files = glob.glob(sys.argv[1] + "/deploy/grafana/*.json") + [sys.argv[1] + "/deploy/grafana-dashboard.json"]
def walk(ps):
    for p in ps:
        yield p
        yield from walk(p.get("panels", []))
n = 0
for f in files:
    d = json.load(open(f))
    exprs = [t["expr"] for p in walk(d["panels"]) for t in p.get("targets", [])]
    exprs += [a["expr"] for a in d.get("annotations", {}).get("list", []) if "expr" in a]
    for v in d["templating"]["list"]:
        q = v.get("definition", "")
        m = re.match(r"label_values\((.*), *\w+\)$", q)
        if m:
            exprs.append(m.group(1))
    for e in exprs:
        e = (e.replace("$__range", "1h").replace("$__rate_interval", "5m").replace("$__interval", "1m")
             .replace("${device}", "x").replace("$device", "x").replace("$group", "x").replace("$interface", "x").replace("$port", "x"))
        n += 1
        rules.append({"record": "r%d" % n, "expr": e})
# JSON is valid YAML, so no PyYAML is needed.
json.dump({"groups": [{"name": "dashboards", "rules": rules}]}, open(sys.argv[2], "w"), indent=1)
print(n, "exprs")
