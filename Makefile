.PHONY: test test-race test-lab test-lab-parallel vet vet-lab cover check build

# The gate to run before every commit: unit tests plus the static checks CI
# runs. Keeps local and CI behaviour identical.
check: vet vet-lab test-race

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
# A full run takes about 37 minutes (76 tests, each resetting the pair to
# baseline in ~20s), so the timeout is 60m: Go's default and the old 30m both
# kill the run partway through.
LAB_TEST_FLAGS = -tags lab -race -count=1 -p 1 -timeout 60m

test-lab:
	MTHA_LAB=1 go test $(LAB_TEST_FLAGS) -v -run '^TestLab' ./...

# The same suite split across several lab instances at once, one shard per
# instance, each instance already up (./testlab/lab.sh up N for each). It
# fails if any shard fails or if the shards did not between them run every
# listed test exactly once; see testlab/shard.sh. An accelerator: test-lab
# stays the authority. Four is the default because the README's measurements
# show four concurrent suites cost nothing (resets 16-19s against 15-17s
# alone) while six to eight saturate the host; count any other suites
# already running on this host towards that four. Pick instances with
#   make test-lab-parallel LAB_INSTANCES="4 5 6 7"
LAB_INSTANCES ?= 1 2 3 4

test-lab-parallel:
	LAB_TEST_FLAGS="$(LAB_TEST_FLAGS)" sh testlab/shard.sh $(LAB_INSTANCES)

vet:
	go vet ./...

# The lab tests carry the `lab` build tag, so plain vet and test never compile
# them and a build break there would go unseen until a live run. Compiling
# with the tag needs no lab.
vet-lab:
	go vet -tags lab ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# Stamp the binary with the nearest tag (e.g. v0.1.0, or v0.1.0-3-gabc1234
# between releases) so `mtha -version` identifies it.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" ./cmd/mtha
