SHELL := bash

GO ?= go
BIN_NAME ?= go-contactd
BIN_DIR ?= bin
DIST_DIR ?= dist
PKG ?= ./cmd/contactd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf 'dev')
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
STAMP_LDFLAGS = -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: build build-static build-static-ext clean dist fmt lint release test vet verify

build:
	mkdir -p "$(BIN_DIR)"
	./build_dist.sh -o "$(BIN_DIR)/$(BIN_NAME)" "$(PKG)"

build-static:
	mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w $(STAMP_LDFLAGS)" -o "$(BIN_DIR)/$(BIN_NAME)" "$(PKG)"

build-static-ext:
	mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -extldflags '-static' $(STAMP_LDFLAGS)" -o "$(BIN_DIR)/$(BIN_NAME)" "$(PKG)"

dist:
	./build_release.sh

release: dist

fmt:
	gofmt -w .

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; skipping"; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

verify: fmt lint vet test

clean:
	rm -rf "$(BIN_DIR)" "$(DIST_DIR)"
