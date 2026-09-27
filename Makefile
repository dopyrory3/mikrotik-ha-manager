.PHONY: test test-race test-lab vet cover check build

# The gate to run before every commit: unit tests plus the static checks CI
# runs. Keeps local and CI behaviour identical.
check: vet test-race

test:
	go test ./...

# The poller and UI models use goroutines, so run the race detector in CI.
test-race:
	go test -race ./...

# The live-router suite (internal/labtest): writes to the testlab/ pair and
# resets it between tests, so it needs the lab up and provisioned first:
#   docker compose -f testlab/docker-compose.yml up -d --build
#   ./testlab/provision.sh
# or ./testlab/lab.sh up. For another lab instance (see README.md):
#   ./testlab/lab.sh up 2 && MTHA_LAB_INSTANCE=2 make test-lab
# -p 1 runs one package's routers at a time (the harness also locks).
test-lab:
	MTHA_LAB=1 go test -tags lab -race -count=1 -p 1 -timeout 30m -v -run '^TestLab' ./...

vet:
	go vet ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# Stamp the binary with the nearest tag (e.g. v0.1.0, or v0.1.0-3-gabc1234
# between releases) so `mtha -version` identifies it.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" ./cmd/mtha
