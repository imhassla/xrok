#!/usr/bin/env bash
# Compare local benchmark artifacts with a checked-in baseline.
set -euo pipefail
BASELINE=${BASELINE:-bench/baseline.json}
SCALE=${SCALE:-bench/results-scale.json}
APPLES=${APPLES:-bench/results.csv}
python3 - "$BASELINE" "$SCALE" "$APPLES" <<'PY'
import csv, json, os, sys
base_path, scale_path, apples_path = sys.argv[1:]
base = json.load(open(base_path))
failed = []
if os.path.exists(scale_path):
    rows = json.load(open(scale_path))
    limits = base.get("scale", {})
    for row in rows:
        lim = limits.get(str(row.get("tunnels")))
        if not lim: continue
        for key in ("put_ms", "get_ms"):
            if float(row.get(key, 0)) > float(lim.get(key, float("inf"))):
                failed.append(f"scale {row['tunnels']} {key}={row[key]} > {lim[key]}")
else:
    print(f"warning: {scale_path} not found; run scripts/synthetic-scale.sh")
if os.path.exists(apples_path):
    vals = {}
    with open(apples_path, newline="") as f:
        for r in csv.DictReader(f):
            try: vals.setdefault(r["implementation"], []).append(float(r["latency_ms"]))
            except (KeyError, ValueError): pass
    if vals.get("xrok") and vals.get("frp"):
        x = sum(vals["xrok"])/len(vals["xrok"]); f = sum(vals["frp"])/len(vals["frp"])
        pct = (x/f-1)*100 if f else 0
        print(f"apples mean latency: xrok={x:.2f}ms frp={f:.2f}ms delta={pct:+.1f}%")
        if pct > float(base.get("apples", {}).get("max_latency_regression_pct", 25)):
            failed.append(f"apples latency regression {pct:.1f}%")
else:
    print(f"warning: {apples_path} not found; run scripts/benchmark-apples.sh")
if failed:
    print("REGRESSION FAILED")
    print("\n".join("- "+x for x in failed))
    sys.exit(1)
print("benchmark regression: PASS")
PY
