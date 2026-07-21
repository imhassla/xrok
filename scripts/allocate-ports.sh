#!/usr/bin/env bash
# Print a collision-free set of host ports for a local Compose lab.
set -euo pipefail
count="${1:-4}"
python3 - "$count" <<'PY'
import socket, sys
n=int(sys.argv[1])
ports=[]
for i in range(n):
    p = None
    for _ in range(32):
        try:
            s=socket.socket(); s.bind(('127.0.0.1', 0)); candidate=s.getsockname()[1]; s.close()
        except OSError:
            # Restricted CI sandboxes may deny bind(); retain deterministic,
            # disjoint values for config validation. Docker remains the
            # authority and the lab runner retries publish collisions.
            candidate=18000 + (int(__import__('os').getpid()) % 1000) + i
        if candidate not in ports:
            p = candidate
            break
    if p is None: raise SystemExit('could not allocate unique ports')
    ports.append(p)
names=['XROK_HA_PORT_A','XROK_HA_UDP_A','XROK_HA_PORT_B','XROK_HA_UDP_B']
for k,v in zip(names,ports): print(f'{k}={v}')
PY
