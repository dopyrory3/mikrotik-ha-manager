.PHONY: test test-race vet cover check build

# The gate to run before every commit: unit tests plus the static checks CI
# runs. Keeps local and CI behaviour identical.
check: vet test-race

test:
	go test ./...

# The poller and UI models use goroutines, so run the race detector in CI.
test-race:
	go test -race ./...

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
