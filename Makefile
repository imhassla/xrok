.PHONY: build test race vet bench regression e2e e2e-soak fuzz security fault-injection api-compat ci-local soak supply-chain lab-parallel lab-smoke otlp-integration turn-integration check

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

ports:
	./scripts/allocate-ports.sh

bench:
	$(GO_ENV) go test -run '^$$' -bench=. -benchmem ./...

regression:
	./scripts/benchmark-regression.sh

soak:
	./scripts/soak-local.sh

supply-chain:
	./scripts/supply-chain-local.sh

# Runs HA, NAT and observability reference labs concurrently in isolated Compose projects.
lab-parallel:
	./scripts/lab-parallel.sh

lab-smoke:
	./scripts/tests/lab-orchestration-smoke.sh

# Runs the real async exporter against the pinned local OTEL Collector.
otlp-integration:
	./scripts/otel-collector-integration.sh

# Exercises TURN-over-UDP/TCP/TLS and ChannelBind against pinned coturn.
turn-integration:
	./scripts/turn-integration.sh

e2e:
	$(GO_ENV) go test -tags=e2e ./e2e -count=1

e2e-soak:
	./scripts/e2e-soak.sh

check: test vet race
