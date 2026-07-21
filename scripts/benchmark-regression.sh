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
            try:
                impl = r["implementation"]
                latency = float(r["latency_ms"])
                code = int(r.get("http_code", "0"))
                size = float(r.get("bytes", "0"))
                vals.setdefault(impl, []).append((latency, code, size))
            except (KeyError, ValueError): pass
    def percentile(xs, p):
        xs = sorted(xs)
        if not xs: return 0.0
        k = (len(xs)-1) * p / 100.0; lo = int(k); hi = min(lo+1, len(xs)-1)
        return xs[lo] + (xs[hi]-xs[lo])*(k-lo)
    summary = {}
    for impl, records in vals.items():
        lat = [x[0] for x in records]; ok = [x for x in records if 200 <= x[1] < 400]
        summary[impl] = {"count": len(records), "p50_ms": percentile(lat,50), "p95_ms": percentile(lat,95), "p99_ms": percentile(lat,99),
                         "error_rate": (len(records)-len(ok))/len(records) if records else 0,
                         "throughput_bytes_per_sec": sum(x[2] for x in ok) / (sum(x[0] for x in ok)/1000) if ok and sum(x[0] for x in ok) else 0}
        print(f"{impl}: p50={summary[impl]['p50_ms']:.2f}ms p95={summary[impl]['p95_ms']:.2f}ms p99={summary[impl]['p99_ms']:.2f}ms errors={summary[impl]['error_rate']:.1%} throughput={summary[impl]['throughput_bytes_per_sec']:.0f}B/s")
    if os.environ.get("ANALYTICS_OUT"):
        json.dump(summary, open(os.environ["ANALYTICS_OUT"], "w"), indent=2)
    if summary.get("xrok") and summary.get("frp"):
        x = summary["xrok"]; f = summary["frp"]
        pct = (x["p95_ms"]/f["p95_ms"]-1)*100 if f["p95_ms"] else 0
        print(f"apples p95 latency delta: {pct:+.1f}%")
        lim = base.get("apples", {})
        if pct > float(lim.get("max_p95_latency_regression_pct", lim.get("max_latency_regression_pct", 25))): failed.append(f"apples p95 latency regression {pct:.1f}%")
        if x["error_rate"] > float(lim.get("max_error_rate", 0.05)): failed.append(f"xrok error rate {x['error_rate']:.1%}")
else:
    print(f"warning: {apples_path} not found; run scripts/benchmark-apples.sh")
if failed:
    print("REGRESSION FAILED")
    print("\n".join("- "+x for x in failed))
    sys.exit(1)
print("benchmark regression: PASS")
PY
