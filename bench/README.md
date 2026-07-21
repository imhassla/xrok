# Reproducible benchmark lab

The lab pins `frps` v0.61.1, coturn 4.6.3 and the OpenTelemetry Collector 0.123.0. It runs alongside a locally built xrok server and provides health checks for every control-plane service.

Generate a development certificate in `local-certs/` (the same files used by `compose.local.yml`), then validate and start:

```sh
./scripts/benchmark-lab.sh config
./scripts/benchmark-lab.sh up
./scripts/benchmark-lab.sh ps
```

Run the existing benchmark client against `localhost:${XROK_PORT:-8443}` and compare with frp on port `${FRP_PORT:-7000}`. Collector traces/metrics are printed by the `debug` exporter. Stop the lab with `./scripts/benchmark-lab.sh down`.

All image tags and service ports are explicit so CI or a later performance run can reproduce the same topology. The compose file does not publish credentials beyond the local benchmark network.
