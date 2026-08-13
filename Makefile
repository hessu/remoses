# remoses
#
# Plain go commands work fine; this exists so the common ones are one word and
# the release builds are reproducible across machines.

BIN        := remoses
CMD        := ./cmd/remoses
CLI_BIN    := remoses-cli
CLI_CMD    := ./cmd/remoses-cli
BUILD_DIR  := build
DIST_DIR   := dist
# Archives are staged somewhere the cross binaries are not, because one of them
# is called `remoses` and so is the directory an archive unpacks to. Staging in
# place deleted the daemon and shipped archives with only the CLI in them.
STAGE_DIR  := dist/.stage

# name:package pairs, so the build and cross targets iterate rather than
# repeating themselves once per binary.
TARGETS    := $(BIN):$(CMD) $(CLI_BIN):$(CLI_CMD)

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS    ?=
LDFLAGS    := -s -w -X main.version=$(VERSION)

# The release targets, as os/arch[/goarm]: the platforms remoses is meant to run
# on — a Pi at the radio site, a Linux or Windows shack PC, a Mac. CGO is off
# everywhere, which is the whole point of the dependency choices: one static
# binary per platform, nothing to install beside it.
#
# Both 32-bit ARM entries are Raspberry Pi, and they are not interchangeable:
#
#   armv6   Pi 1, Pi Zero and Zero W
#   armv7   Pi 2, and any later Pi running a 32-bit Raspberry Pi OS
#   arm64   Pi 3, 4, 5 and Zero 2 W on a 64-bit Raspberry Pi OS
#
# GOARM is written down rather than left to the toolchain because its default
# has changed between Go releases, and a v7 binary does not run on a Pi Zero —
# it dies with an illegal instruction, which is a miserable thing to debug at a
# remote site.
PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	linux/arm/7 \
	linux/arm/6 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64 \
	windows/arm64

.PHONY: all build test race cover vet fmt fmt-check lint check run config-check \
        generate spec-check cross release clean tidy help

all: build ## Build the binaries

build: ## Build remoses and remoses-cli into build/
	@mkdir -p $(BUILD_DIR)
	@for t in $(TARGETS); do \
		name=$${t%:*}; pkg=$${t#*:}; \
		CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
			-o $(BUILD_DIR)/$$name $$pkg || exit 1; \
		echo "built $(BUILD_DIR)/$$name ($(VERSION))"; \
	done

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

# internal/wire is generated from api/openapi.yaml and checked in, so a build
# needs neither the generator nor the network. The generator itself is a tool
# dependency in go.mod, so `go tool` runs the version go.sum pins rather than
# whatever is on the machine.
generate: ## Regenerate internal/wire from api/openapi.yaml
	go tool oapi-codegen -config api/codegen.yaml api/openapi.yaml
	@echo "generated internal/wire/wire.gen.go"

# The half of "the spec is the contract" that a test cannot cover: the tests
# prove the daemon agrees with the checked-in Go, and this proves the checked-in
# Go is still what the document says. Without it an edit to openapi.yaml that
# nobody regenerated would leave the two describing different APIs, with
# everything passing.
# It regenerates in place and puts the file back, rather than generating
# somewhere else and comparing: the output path is in api/codegen.yaml and the
# config wins over -o, so "somewhere else" would silently compare against
# nothing at all. Whatever happens, the working tree is left as it was found.
spec-check: ## Fail if internal/wire is stale with respect to the spec
	@out=internal/wire/wire.gen.go; saved=$$(mktemp); cp $$out $$saved; \
	if ! go tool oapi-codegen -config api/codegen.yaml api/openapi.yaml; then \
		cp $$saved $$out; rm -f $$saved; exit 1; \
	fi; \
	if diff -q $$saved $$out >/dev/null; then \
		rm -f $$saved; echo "internal/wire matches api/openapi.yaml"; \
	else \
		diff -u $$saved $$out | head -40; \
		cp $$saved $$out; rm -f $$saved; \
		echo "$$out does not match api/openapi.yaml; run make generate"; \
		exit 1; \
	fi

# What CI should run, and what to run before committing.
check: fmt-check vet spec-check test ## fmt-check + vet + spec-check + test

run: build ## Run against remoses.yaml
	$(BUILD_DIR)/$(BIN) -config remoses.yaml

config-check: build ## Validate remoses.example.yaml
	$(BUILD_DIR)/$(BIN) -config remoses.example.yaml -check

cross: ## Build release binaries for every target platform into dist/
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); \
		arch=$$(echo $$p | cut -d/ -f2); \
		goarm=$$(echo $$p | cut -d/ -f3 -s); \
		label=$$os-$$arch; \
		if [ -n "$$goarm" ]; then label=$$os-armv$$goarm; fi; \
		ext=''; [ "$$os" = windows ] && ext='.exe'; \
		echo "  $$label"; \
		for t in $(TARGETS); do \
			name=$${t%:*}; pkg=$${t#*:}; \
			out=$(DIST_DIR)/$$name-$(VERSION)-$$label$$ext; \
			GOOS=$$os GOARCH=$$arch GOARM=$$goarm CGO_ENABLED=0 \
				go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $$pkg || exit 1; \
		done; \
	done
	@echo "release binaries in $(DIST_DIR)/"

# release packages what cross built: one archive per platform carrying both
# binaries, the licence, the user guide and the annotated example configuration.
#
# An archive rather than a bare binary because of who downloads this. Somebody
# putting remoses on a Pi needs remoses.example.yaml to get anywhere at all, and
# a tarball preserves the executable bit that a browser download does not.
release: cross ## Package dist/ into per-platform archives with checksums
	@rm -f $(DIST_DIR)/SHA256SUMS
	@rm -rf $(STAGE_DIR)
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); \
		arch=$$(echo $$p | cut -d/ -f2); \
		goarm=$$(echo $$p | cut -d/ -f3 -s); \
		label=$$os-$$arch; \
		if [ -n "$$goarm" ]; then label=$$os-armv$$goarm; fi; \
		ext=''; [ "$$os" = windows ] && ext='.exe'; \
		dir=remoses-$(VERSION)-$$label; \
		stage=$(STAGE_DIR)/$$dir; \
		mkdir -p $$stage; \
		for t in $(TARGETS); do \
			name=$${t%:*}; \
			cp $(DIST_DIR)/$$name-$(VERSION)-$$label$$ext $$stage/$$name$$ext || exit 1; \
		done; \
		cp LICENSE README.md remoses.example.yaml $$stage/ || exit 1; \
		mkdir -p $$stage/docs && cp docs/*.md $$stage/docs/ || exit 1; \
		if [ "$$os" = windows ]; then \
			(cd $(STAGE_DIR) && zip -qr $(CURDIR)/$(DIST_DIR)/$$dir.zip $$dir) || exit 1; \
		else \
			tar -czf $(DIST_DIR)/$$dir.tar.gz -C $(STAGE_DIR) $$dir || exit 1; \
		fi; \
		echo "  packaged $$dir"; \
	done
	@rm -rf $(STAGE_DIR)
	@cd $(DIST_DIR) && (shasum -a 256 *.tar.gz *.zip 2>/dev/null || sha256sum *.tar.gz *.zip) > SHA256SUMS
	@echo "release archives in $(DIST_DIR)/"

tidy: ## go mod tidy
	go mod tidy

clean: ## Remove build output and test caches
	rm -rf $(BUILD_DIR) $(DIST_DIR)
	go clean -testcache

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'
