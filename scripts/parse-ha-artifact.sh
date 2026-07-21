#!/usr/bin/env bash
# Validate and summarize a HA integration artifact.
set -euo pipefail
artifact="${1:-.artifacts/ha-integration.json}"
python3 - "$artifact" <<'PY'
import json, sys
p = sys.argv[1]
with open(p) as f: d=json.load(f)
required=("status","attempts","collision_count","selected_ports","compose_up_duration_ms","service_health")
missing=[k for k in required if k not in d]
if missing: raise SystemExit("missing fields: "+", ".join(missing))
ports=d["selected_ports"]
if any(k not in ports for k in ("tcp_a","udp_a","tcp_b","udp_b")): raise SystemExit("incomplete selected_ports")
print("status=%s attempts=%s collisions=%s up_ms=%s" % (d["status"], d["attempts"], d["collision_count"], d["compose_up_duration_ms"]))
print("ports=tcp:%s/%s udp:%s/%s services=%s" % (ports["tcp_a"], ports["tcp_b"], ports["udp_a"], ports["udp_b"], len(d["service_health"])))
PY
