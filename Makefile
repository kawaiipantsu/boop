# Boop — see PROJECT.md §36 for the required target contract.

BINARY      := boop
MODULE      := github.com/kawaiipantsu/boop
VERSION     ?= $(shell sed -n 's/^## \[\([0-9][^]]*\)\].*/\1/p' CHANGELOG.md 2>/dev/null | head -1)
VERSION     := $(if $(VERSION),$(VERSION),0.1.0-dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY       := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo true || echo false)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG         := $(MODULE)/internal/version

LDFLAGS := -s -w \
	-X '$(PKG).Version=$(VERSION)' \
	-X '$(PKG).Commit=$(COMMIT)' \
	-X '$(PKG).Dirty=$(DIRTY)'
# SOURCE_DATE_EPOCH callers get reproducible builds; otherwise stamp the date.
ifndef SOURCE_DATE_EPOCH
LDFLAGS += -X '$(PKG).Date=$(DATE)'
endif

GO       ?= go
DIST     := dist
COVER    := coverage.out
CMD      := ./cmd/boop
GOFILES  := $(shell find . -name '*.go' -not -path './vendor/*' 2>/dev/null)

.DEFAULT_GOAL := help

## ---------------------------------------------------------------- help

.PHONY: help
help: ## Show available targets
	@echo "Development:"
	@echo "  run                 Run Boop"
	@echo "  deps                Download and verify modules"
	@echo "  fmt                 Format code"
	@echo "  vet                 Run go vet"
	@echo "  lint                Run linter (golangci-lint when available)"
	@echo "  test                Run test suite"
	@echo "  test-unit           Run unit tests only"
	@echo "  test-integration    Run integration tests"
	@echo "  test-e2e            Run end-to-end tests"
	@echo "  race                Run tests with the race detector"
	@echo "  bench               Run benchmarks"
	@echo "  fuzz                Run fuzz targets (FUZZTIME each, default 20s)"
	@echo "  coverage            Generate coverage report"
	@echo ""
	@echo "Build:"
	@echo "  build               Build host binary"
	@echo "  build-all           Cross-build supported CLI/TUI binaries"
	@echo "  build-linux         Build linux amd64+arm64"
	@echo "  build-darwin        Build darwin amd64+arm64"
	@echo "  build-windows       Build windows amd64+arm64"
	@echo "  web                 Build and embed the WebUI"
	@echo "  dist                Create distributable archives"
	@echo "  install             Install boop into GOPATH/bin"
	@echo "  clean               Remove generated files"
	@echo ""
	@echo "Release:"
	@echo "  release-check       Verify release readiness"
	@echo "  snapshot            Create local snapshot artifacts"
	@echo "  security            Run vulnerability scan"
	@echo ""
	@echo "Version: $(VERSION)  Commit: $(COMMIT)"

## ---------------------------------------------------------------- dev

.PHONY: deps
deps: ## Download and verify modules
	$(GO) mod download
	$(GO) mod verify

.PHONY: fmt
fmt: ## Format code
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail when code is not formatted
	@unformatted=$$(gofmt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run linter when installed, otherwise fall back to vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; running go vet"; $(GO) vet ./...; \
	fi

.PHONY: test
test: ## Run the full test suite
	$(GO) test ./...

.PHONY: test-unit
test-unit: ## Run unit tests only
	$(GO) test ./cmd/... ./internal/... ./tui/... ./web/...

.PHONY: test-integration
test-integration: ## Run integration tests
	$(GO) test -tags=integration ./tests/integration/...

.PHONY: test-e2e
test-e2e: ## Run end-to-end tests
	$(GO) test -tags=e2e ./tests/e2e/...

.PHONY: race
race: ## Run tests with the race detector
	$(GO) test -race ./...

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -run '^$$' -bench=. -benchmem ./...

.PHONY: fuzz
FUZZTIME ?= 20s
fuzz: ## Run each fuzz target for $(FUZZTIME) (override with FUZZTIME=)
	$(GO) test -run '^$$' -fuzz='^FuzzParseRenderRoundTrip$$' -fuzztime=$(FUZZTIME) ./internal/project/
	$(GO) test -run '^$$' -fuzz='^FuzzClassifyCommand$$' -fuzztime=$(FUZZTIME) ./internal/permissions/

.PHONY: coverage
coverage: ## Generate coverage report
	$(GO) test -coverprofile=$(COVER) -covermode=atomic ./...
	$(GO) tool cover -func=$(COVER) | tail -1
	$(GO) tool cover -html=$(COVER) -o coverage.html
	@echo "wrote coverage.html"

.PHONY: run
run: ## Run Boop
	$(GO) run -ldflags "$(LDFLAGS)" $(CMD) $(ARGS)

## ---------------------------------------------------------------- build

.PHONY: build
build: ## Build the host binary
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

# $(1)=os $(2)=arch
define build_target
	@echo "building $(1)/$(2)"
	@mkdir -p $(DIST)/$(BINARY)_$(VERSION)_$(1)_$(2)
	CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)_$(VERSION)_$(1)_$(2)/$(BINARY)$(if $(filter windows,$(1)),.exe,) $(CMD)
endef

.PHONY: build-linux-amd64
build-linux-amd64: ; $(call build_target,linux,amd64)
.PHONY: build-linux-arm64
build-linux-arm64: ; $(call build_target,linux,arm64)
.PHONY: build-darwin-amd64
build-darwin-amd64: ; $(call build_target,darwin,amd64)
.PHONY: build-darwin-arm64
build-darwin-arm64: ; $(call build_target,darwin,arm64)
.PHONY: build-windows-amd64
build-windows-amd64: ; $(call build_target,windows,amd64)
.PHONY: build-windows-arm64
build-windows-arm64: ; $(call build_target,windows,arm64)

.PHONY: build-linux build-darwin build-windows build-all
build-linux: build-linux-amd64 build-linux-arm64 ## Build linux targets
build-darwin: build-darwin-amd64 build-darwin-arm64 ## Build darwin targets
build-windows: build-windows-amd64 build-windows-arm64 ## Build windows targets
build-all: build-linux build-darwin build-windows ## Cross-build every target

.PHONY: web web-build web-clean
web: web-build ## Build and embed the WebUI
web-build: ## Build the WebUI bundle
	@scripts/build-web.sh
web-clean: ## Remove WebUI build output
	@rm -rf web/static/dist

.PHONY: install
install: ## Install boop into GOPATH/bin
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" $(CMD)

.PHONY: clean
clean: ## Remove generated files
	rm -rf $(DIST) $(BINARY) $(BINARY).exe $(COVER) coverage.html
	$(GO) clean -cache -testcache >/dev/null 2>&1 || true

## ---------------------------------------------------------------- release

.PHONY: dist
dist: build-all ## Create distributable archives with checksums
	@scripts/package.sh $(DIST) $(BINARY) $(VERSION)

.PHONY: snapshot
snapshot: ## Create local snapshot artifacts
	@$(MAKE) dist VERSION=$(VERSION)-snapshot.$(COMMIT)

.PHONY: release-check
release-check: fmt-check vet lint test build-all ## Verify release readiness
	@scripts/release-check.sh $(VERSION)

.PHONY: security
security: ## Run vulnerability scan
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck ./...; \
	else echo "govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@latest"; fi

.PHONY: licenses
licenses: ## List module licenses
	$(GO) list -m -f '{{.Path}} {{.Version}}' all

.PHONY: generate
generate: ## Run go generate
	$(GO) generate ./...
