.PHONY: build test race vet bench regression e2e fuzz security api-compat ci-local check

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

security: vet fuzz

api-compat:
	./scripts/api-snapshot.sh

ci-local:
	./scripts/ci-local.sh

bench:
	$(GO_ENV) go test -run '^$$' -bench=. -benchmem ./...

regression:
	./scripts/benchmark-regression.sh

e2e:
	$(GO_ENV) go test -tags=e2e ./e2e -count=1

check: test vet race
