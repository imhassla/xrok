# Reproducible benchmark lab

The lab pins `frps` v0.61.1, coturn 4.6.3 and the OpenTelemetry Collector 0.123.0. It runs alongside a locally built xrok server and provides health checks for every control-plane service.

Generate a development certificate in `local-certs/` (the same files used by `compose.local.yml`), then validate and start:

```sh
./scripts/benchmark-lab.sh config
./scripts/benchmark-lab.sh up
./scripts/benchmark-lab.sh ps
```

Run the existing benchmark client against `localhost:${XROK_PORT:-8443}` and compare with frp on port `${FRP_PORT:-7000}`. Collector traces/metrics are printed by the `debug` exporter. Stop the lab with `./scripts/benchmark-lab.sh down`.

To verify the complete telemetry path independently, run
`make otlp-integration`. It allocates collision-resistant local ports, starts the
pinned collector, emits a traced request through `ServerRuntime`, waits for the
asynchronous exporter to drain, checks collector output, and removes its
isolated Compose project.

## Apples-to-apples runner

Expose the same application payload through xrok and frp (for example on
ports 18080 and 18081), then run:

```sh
ITERATIONS=20 XROK_URL=http://127.0.0.1:18080 FRP_URL=http://127.0.0.1:18081 \
  ./scripts/benchmark-apples.sh
```

The runner records per-request latency, status and payload size in
`bench/results.csv`. This compares workload endpoints rather than frp's
control port. CPU/RSS can be sampled with `docker stats --no-stream` while it
runs; pin compose image tags before publishing a result. Failed requests are
recorded as HTTP code `000`, keeping reconnect/flakiness visible, and make the
runner fail by default. `BENCHMARK_ALLOW_ERRORS=1` is reserved for explicit
failure-behavior measurements. The regression gate also fails when either the
scale or apples-to-apples evidence is missing.

All image tags and service ports are explicit so CI or a later performance run can reproduce the same topology. The compose file does not publish credentials beyond the local benchmark network.

## Local HA lab

`./scripts/ha-local.sh config` validates the two-replica HA exercise without
starting containers. `up`/`down` manage pinned Redis 7.4.2 and PostgreSQL 16.6
dependencies plus two xrok server replicas; `smoke` starts the stack and checks
all healthchecks. Redis/Postgres endpoints are supplied through
`XROK_HA_REDIS_ADDR` and `XROK_HA_POSTGRES_DSN` for injectable state-store and
failover tests.

For the real Docker integration (adapter-style load plus transient database
failures), run `make ha-integration`. It performs bounded writes against both
services, stops/starts each dependency, verifies persistence and replica
recovery, and writes `.artifacts/ha-integration.json`:

```sh
HA_INTEGRATION_LOAD_ITERATIONS=250 HA_SMOKE_RETRIES=60 make ha-integration
```

Lab artifacts use `xrok.lab-result/v1`; the parallel orchestrator writes an
`xrok.lab-summary/v1` result and rejects missing, malformed, or failed child
artifacts. `make lab-smoke` validates those contracts without Docker or
network access.
