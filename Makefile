SHELL := bash

GO ?= go
BIN_NAME ?= go-contactd
BIN_DIR ?= bin
DIST_DIR ?= dist
PKG ?= ./cmd/contactd

.PHONY: build build-static clean dist fmt lint release test vet verify

build:
	mkdir -p "$(BIN_DIR)"
	./build_dist.sh -o "$(BIN_DIR)/$(BIN_NAME)" "$(PKG)"

build-static:
	mkdir -p "$(BIN_DIR)"
	./build_dist.sh --strip -o "$(BIN_DIR)/$(BIN_NAME)" "$(PKG)"

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
