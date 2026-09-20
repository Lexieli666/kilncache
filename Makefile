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
RESULTS_DIR := bench/results/$(TODAY)-$(HOSTNAME_S)

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
cover: ## Coverage profile and summary
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Coverage as HTML
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: integration
integration: ## Integration tests (starts real servers; needs Docker for the cluster suite)
	$(GO) test -tags=integration -timeout=20m ./tests/integration/...

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
	$(BIN_DIR)/kilnbench -smoke -out $(RESULTS_DIR)

.PHONY: bench
bench: build ## Full benchmark matrix (long)
	$(BIN_DIR)/kilnbench -out $(RESULTS_DIR)

.PHONY: chaos
chaos: build ## Chaos run against the compose cluster
	$(BIN_DIR)/kilnchaos -duration 10m -out $(RESULTS_DIR)

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
