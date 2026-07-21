#!/usr/bin/env bash
# Validate and summarize a HA integration artifact.
set -euo pipefail
artifact="${1:-.artifacts/ha-integration.json}"
python3 - "$artifact" <<'PY'
import json, sys
p = sys.argv[1]
with open(p) as f: d=json.load(f)
required=("schema_version","lab","status","attempts","collision_count","selected_ports","compose_up_duration_ms","service_health")
missing=[k for k in required if k not in d]
if missing: raise SystemExit("missing fields: "+", ".join(missing))
if d["schema_version"] != "xrok.lab-result/v1" or d["lab"] != "ha": raise SystemExit("unsupported HA artifact schema")
if d["status"] not in ("passed","failed","skipped"): raise SystemExit("invalid status")
ports=d["selected_ports"]
if any(k not in ports for k in ("tcp_a","udp_a","tcp_b","udp_b")): raise SystemExit("incomplete selected_ports")
if d["status"] == "passed":
    values=[str(ports[k]) for k in ("tcp_a","udp_a","tcp_b","udp_b")]
    if any(not x.isdigit() or not (0 < int(x) < 65536) for x in values): raise SystemExit("invalid selected port")
print("status=%s attempts=%s collisions=%s up_ms=%s" % (d["status"], d["attempts"], d["collision_count"], d["compose_up_duration_ms"]))
print("ports=tcp:%s/%s udp:%s/%s services=%s" % (ports["tcp_a"], ports["tcp_b"], ports["udp_a"], ports["udp_b"], len(d["service_health"])))
if d["status"] == "failed": raise SystemExit(1)
PY
