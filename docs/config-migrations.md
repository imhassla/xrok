# Configuration schema and migrations

Configuration files carry a numeric `version`. The current schema is **v2**
(`config.CurrentVersion`). Files without a version are treated as v0 and are
migrated in memory when loaded.

Schema v2 adds the optional `telemetry` block for bounded asynchronous
OTLP/HTTP trace export. Migrating v0/v1 files fills queue, batch, retry and
shutdown defaults but leaves `telemetry.endpoint` empty, so export remains
disabled until explicitly configured. The standard
`OTEL_EXPORTER_OTLP_ENDPOINT`/`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` variables and
the `XROK_OTLP_*` resource-bound overrides are supported by the server CLI.

Migration is deterministic and idempotent. Client v0 files that set the legacy
`connection_type` field are mapped to `transport` (`tls` becomes `auto`,
`quic` remains `quic`); saving the file writes the
canonical v2 representation. Server v0 files gain the memory state-backend
default and the disabled-by-default telemetry resource bounds.
Both loaders and save paths reject versions newer than the running binary, so
an older process cannot silently discard fields introduced by a future schema.

Upgrade workflow:

1. Keep a backup of the existing YAML.
2. Run the client/server with the file once (loading performs migration).
3. Use the config save path or the explicit migration tooling in deployment
   automation to persist v2.
4. Validate with `go test ./cmd/config` and `make api-compat` before rollout.

Future schema changes must add a monotonic migration step, preserve unknown
fields where possible, and include an idempotence test and a fixture in
`cmd/config`.
