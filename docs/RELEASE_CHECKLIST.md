# Local release checklist

- [ ] `make check` (unit, race, vet)
- [ ] `make e2e` and `make regression`
- [ ] `make api-compat` (public API snapshot)
- [ ] `make security` and dependency/SBOM review
- [ ] Review `docs/config-migrations.md` for schema changes
- [ ] Update `CHANGELOG.md` and version metadata
- [ ] Build and smoke-test Docker/reference labs
- [ ] Commit locally; publish only after explicit approval
