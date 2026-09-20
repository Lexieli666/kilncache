# KilnCache developer entry points.
#
# Every target here is also what CI runs, so "it passes on my machine" and "it
# passes in CI" are the same command. Nothing in this file prints a number that
# is not produced by the command it runs.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE      := github.com/Lexieli666/kilncache
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -X $(MODULE)/internal/buildinfo.Version=$(VERSION) -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT)
GO          ?= go
GOTESTFLAGS ?=
COMPOSE     := docker compose -f deploy/compose/docker-compose.yml
HOSTNAME_S  := $(shell hostname -s 2>/dev/null || hostname)
TODAY       := $(shell date -u +%Y-%m-%d)
# Absolute, and quoted at every use.
#
# Absolute because `go test` runs with the package directory as its working
# directory, so a relative results path lands under internal/<pkg>/ instead of
# the repository root -- silently, and only for the targets that go through a
# test binary. Quoted because this checkout's path contains spaces, and an
# unquoted one turns into three arguments.
REPO_ROOT   := $(shell git rev-parse --show-toplevel 2>/dev/null || pwd)
RESULTS_DIR := $(REPO_ROOT)/bench/results/$(TODAY)-$(HOSTNAME_S)

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build all binaries into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/ ./cmd/...

.PHONY: fmt
fmt: ## Rewrite sources with gofmt
	gofmt -w -s $(shell git ls-files '*.go')

.PHONY: fmt-check
fmt-check: ## Fail if any source is not gofmt-clean
	@out=$$(gofmt -l -s $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "gofmt clean"

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: fmt-check vet ## gofmt + go vet + golangci-lint
	golangci-lint run ./...

.PHONY: test
test: ## Unit and property tests
	$(GO) test $(GOTESTFLAGS) ./...

.PHONY: test-race
test-race: ## Unit and property tests under the race detector
	$(GO) test -race $(GOTESTFLAGS) ./...

.PHONY: test-short
test-short: ## Skip slow tests
	$(GO) test -short $(GOTESTFLAGS) ./...

.PHONY: cover
cover: ## Coverage from unit and property tests only
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

# The published coverage number.
#
# `go test -cover ./...` attributes coverage only to the package under test, so
# it scores internal/node and internal/protocol at zero even though the
# integration suite drives every line of them. -coverpkg across the whole module,
# with the integration tag on, measures what is actually exercised. The narrower
# `cover` target stays because it is the fast one for the edit loop.
.PHONY: cover-all
cover-all: ## Coverage across unit, property and integration tests (the published figure)
	$(GO) test -tags=integration -coverpkg=./internal/...,./cmd/... 	  -coverprofile=coverage.out -covermode=atomic -timeout=30m 	  ./internal/... ./cmd/... ./tests/integration/
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Coverage as HTML
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: integration
integration: ## Integration tests (starts real servers; needs Docker for the container suite)
	$(GO) test -tags=integration -timeout=25m ./tests/integration/... ./cmd/chaos/

.PHONY: compose-up
compose-up: ## Start the three-node cluster
	$(COMPOSE) up -d --build

.PHONY: compose-down
compose-down: ## Stop the cluster and remove its named volumes
	$(COMPOSE) down -v

.PHONY: compose-logs
compose-logs: ## Follow cluster logs
	$(COMPOSE) logs -f

.PHONY: bench-smoke
bench-smoke: build ## Quick benchmark run against a locally started cluster
	$(BIN_DIR)/bench -smoke -out "$(RESULTS_DIR)"

.PHONY: bench
bench: build ## Full benchmark matrix (long)
	$(BIN_DIR)/bench -out "$(RESULTS_DIR)"

.PHONY: chaos
chaos: build ## 10-minute chaos run against the compose cluster (needs it running)
	$(BIN_DIR)/chaos -duration 10m -out $(RESULTS_DIR) 	  -compose-file deploy/compose/docker-compose.yml -compose-project kilncache

.PHONY: chaos-smoke
chaos-smoke: build ## Short chaos run, for checking the harness itself
	$(BIN_DIR)/chaos -duration 60s -fault-every 20s -fault-down 8s 	  -converge-wait 60s -out $(RESULTS_DIR) 	  -compose-file deploy/compose/docker-compose.yml -compose-project kilncache

.PHONY: quota-report
quota-report: ## Measure quota, eviction and index-scan behaviour into bench/results/
	KILNCACHE_RESULTS_DIR="$(RESULTS_DIR)" $(GO) test -v -count=1 \
	  -run 'TestWriteQuotaReport|TestIndexScanIsNotBlockedByWrites' ./internal/storage/

.PHONY: device-baseline
device-baseline: ## fio device baseline into bench/results/
	scripts/device-baseline.sh

.PHONY: hostinfo
hostinfo: ## Record host information into bench/results/
	scripts/hostinfo.sh

.PHONY: fixture
fixture: ## Generate the Bazel C++ benchmark workspace
	scripts/gen-bazel-fixture.sh

.PHONY: clean
clean: ## Remove build output and coverage artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.html

.PHONY: tools
tools: ## Report the versions of every external tool this repo uses
	scripts/toolcheck.sh

.PHONY: check-numbers
check-numbers: ## Fail if any published number lacks a raw result file (CONTRIBUTING rule 1)
	scripts/check-numbers.sh

.PHONY: placement-report
placement-report: ## Measure placement balance and key movement into bench/results/
	KILNCACHE_RESULTS_DIR="$(RESULTS_DIR)" $(GO) test -run TestWritePlacementReport -v -count=1 ./internal/cluster/
