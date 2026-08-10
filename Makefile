GO ?= go

PROJECT_TMP ?= $(CURDIR)/.tmp
TMPDIR ?= $(PROJECT_TMP)/tmp
GOCACHE ?= $(PROJECT_TMP)/go-build
GOLANGCI_LINT_CACHE ?= $(PROJECT_TMP)/golangci-lint
export TMPDIR
export GOCACHE
export GOLANGCI_LINT_CACHE

.PHONY: all tempdirs build test test-race check check-fmt gofix vet lint mod-verify vulncheck fix

all: check

tempdirs:
	@mkdir -p "$(TMPDIR)" "$(GOLANGCI_LINT_CACHE)"
	@if [ "$(GOCACHE)" != "off" ]; then mkdir -p "$(GOCACHE)"; fi

build: tempdirs
	$(GO) build ./...

test: tempdirs
	$(GO) test ./...

test-race: tempdirs
	$(GO) test -race ./...

check: check-fmt gofix vet build test-race lint mod-verify vulncheck
	@echo "check: all gates passed"

check-fmt:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; \
	fi

vet: tempdirs
	$(GO) vet ./...

gofix: tempdirs
	$(GO) fix ./...

GOLANGCI_LINT_VERSION ?= v2.12.2

lint: tempdirs
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found, installing $(GOLANGCI_LINT_VERSION)..."; \
		curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$$(go env GOPATH)/bin" "$(GOLANGCI_LINT_VERSION)"; \
	fi
	golangci-lint run ./...

mod-verify: tempdirs
	$(GO) mod verify

vulncheck: tempdirs
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "govulncheck not found, installing..."; \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@latest; \
	fi
	govulncheck ./...

fix: tempdirs
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found, installing $(GOLANGCI_LINT_VERSION)..."; \
		curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$$(go env GOPATH)/bin" "$(GOLANGCI_LINT_VERSION)"; \
	fi
	-golangci-lint run --fix ./...
	$(GO) mod tidy
