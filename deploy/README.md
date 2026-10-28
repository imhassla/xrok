# Deploying xrok (bare metal, systemd)

Built locally, never on the server. `install.sh` cross-compiles
linux/amd64, copies the binary and these files, and (re)starts the service.

```sh
deploy/install.sh ubuntu@52.77.122.131 ~/Documents/Singa-btfs.pem
```

First run creates the `xrok` system user, `/etc/xrok/server.yaml` (from
`server.yaml.example`, with a generated `api_token`), `/var/lib/xrok`,
`/var/log/xrok`. Later runs only replace the binary and unit files.

## Current host: control server 52.77.122.131 (shared with vpn-infra)

| Port | Proto | Purpose | Cloud firewall |
|---|---|---|---|
| 8443 | TCP | control + HTTPS tunnels `https://<name>.xrok.firestarter.click:8443` | open |
| 9000-9019 | TCP | `-R` reverse ports (enforced by `reverse_port_min/max`) | open |
| 8443 | UDP | QUIC transport (optional) | reachable in the 2026-10-28 lab; verify current firewall |
| 4000+, 7000+ | TCP | legacy per-client listeners — not bound (`legacy_listeners: false`) | keep closed |
| 127.0.0.1:6060 | TCP | pprof (`ssh -L 16060:127.0.0.1:6060`) | n/a |

80/443 belong to the vpn-infra nginx; do not bind them directly from xrok.
For a coordinated public 443 rollout with preserved mTLS, see [Shared HTTPS ingress](ingress.md).

- **TLS**: `xrok.firestarter.click` + `*.xrok.firestarter.click` (Let's Encrypt,
  Route53 DNS-01 via the `vpn-wildcard-cert-manager` container's certbot).
  `xrok-cert-sync.timer` runs daily: `certbot renew` + copy to `/etc/xrok/tls`;
  xrok hot-reloads the files, no restart.
- **Egress**: xrok refuses SOCKS/UDP to loopback/private/link-local/own
  addresses; `xrok-egress-fw` adds an iptables owner-match (`XROK_EGRESS`
  chain) on top, installed by `ExecStartPre`.
- **Resources**: `CPUQuota=60%` (30% of a 2 vCPU instance), `CPUWeight=50`,
  `MemoryMax=384M`, `GOMEMLIMIT=200MiB`.
- **Logs**: `/var/log/xrok/server.log`, logrotate daily/50M, 7 kept.

## Client (local)

```sh
make install-local            # -> ~/.local/bin/xrok
cat ~/.xrok/config.yaml       # server, server_port: 8443, token
xrok http 3000 [name]         # https://name.xrok.firestarter.click:8443
xrok tcp 22 9001              # xrok.firestarter.click:9001 -> localhost:22
xrok socks [1080]             # local SOCKS5 through the server
```

Token: `sudo awk '/^api_token:/{print $2}' /etc/xrok/server.yaml` on the server.
