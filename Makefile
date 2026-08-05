# remoses
#
# Plain go commands work fine; this exists so the common ones are one word and
# the release builds are reproducible across machines.

BIN        := remoses
CMD        := ./cmd/remoses
BUILD_DIR  := build
DIST_DIR   := dist

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS    ?=
LDFLAGS    := -s -w -X main.version=$(VERSION)

# The release targets are the platforms remoses is meant to run on: a Pi at the
# radio site, a Linux or Windows shack PC, a Mac. CGO is off everywhere — the
# whole point of the dependency choices is a single static binary per platform.
PLATFORMS := linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64

.PHONY: all build test race cover vet fmt fmt-check lint check run config-check \
        cross clean tidy help

all: build ## Build the binaries

build: ## Build remoses into build/
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BUILD_DIR)/$(BIN) $(CMD)
	@echo "built $(BUILD_DIR)/$(BIN) ($(VERSION))"

test: ## Run the test suite with the race detector
	go test -race -count=1 ./...

cover: ## Run tests and report per-package coverage
	go test -race -count=1 -cover ./...

vet: ## go vet
	go vet ./...

fmt: ## Format the tree in place
	gofmt -w .

fmt-check: ## Fail if anything is unformatted
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

# What CI should run, and what to run before committing.
check: fmt-check vet test ## fmt-check + vet + test

run: build ## Run against remoses.yaml
	$(BUILD_DIR)/$(BIN) -config remoses.yaml

config-check: build ## Validate remoses.example.yaml
	$(BUILD_DIR)/$(BIN) -config remoses.example.yaml -check

cross: ## Build release binaries for every target platform into dist/
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=''; [ "$$os" = windows ] && ext='.exe'; \
		out=$(DIST_DIR)/$(BIN)-$(VERSION)-$$os-$$arch$$ext; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(CMD) || exit 1; \
	done
	@echo "release binaries in $(DIST_DIR)/"

tidy: ## go mod tidy
	go mod tidy

clean: ## Remove build output and test caches
	rm -rf $(BUILD_DIR) $(DIST_DIR)
	go clean -testcache

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'
