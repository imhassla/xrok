.PHONY: build test race vet bench regression e2e fuzz security fault-injection api-compat ci-local soak supply-chain check

GOCACHE ?= /tmp/xrok-go-build
GOMODCACHE ?= /tmp/xrok-go-mod
GO_ENV = GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE)

build:
	$(GO_ENV) go build ./...

test:
	$(GO_ENV) go test ./...

race:
	$(GO_ENV) go test -race ./...

vet:
	$(GO_ENV) go vet ./...

fuzz:
	$(GO_ENV) go test ./cmd/config ./cmd/p2p -run '^$$' -fuzz=Fuzz -fuzztime=10s

fault-injection:
	$(GO_ENV) go test ./cmd/p2p -run 'TestFault' -count=1

security: vet fuzz

api-compat:
	./scripts/api-snapshot.sh

ci-local:
	./scripts/ci-local.sh

# Requires Docker; reports SKIP (success) when no daemon is available.
ha-integration:
	./scripts/ha-docker-integration.sh

bench:
	$(GO_ENV) go test -run '^$$' -bench=. -benchmem ./...

regression:
	./scripts/benchmark-regression.sh

soak:
	./scripts/soak-local.sh

supply-chain:
	./scripts/supply-chain-local.sh

e2e:
	$(GO_ENV) go test -tags=e2e ./e2e -count=1

check: test vet race
