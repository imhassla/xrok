# Configuration schema and migrations

Configuration files carry a numeric `version`. The current schema is **v1**
(`config.CurrentVersion`). Files without a version are treated as v0 and are
migrated in memory when loaded.

Migration is deterministic and idempotent. Client v0 files that set the legacy
`connection_type` field are mapped to `transport`; saving the file writes the
canonical v1 representation. Server v0 files only gain the version marker.

Upgrade workflow:

1. Keep a backup of the existing YAML.
2. Run the client/server with the file once (loading performs migration).
3. Use the config save path or the explicit migration tooling in deployment
   automation to persist v1.
4. Validate with `go test ./cmd/config` and `make api-compat` before rollout.

Future schema changes must add a monotonic migration step, preserve unknown
fields where possible, and include an idempotence test and a fixture in
`cmd/config`.
