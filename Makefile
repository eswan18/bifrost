SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := build

# Where `go install` puts binaries: GOBIN if set, else GOPATH/bin. This is the
# directory `make install` lands ib in, and the one that has to be on PATH.
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR := $(shell go env GOPATH)/bin
endif

.PHONY: run build build-bif install test lint

# static/style.css is hand-written and committed — there is no CSS build step.
run:
	go run ./cmd/bifrost

build:
	CGO_ENABLED=0 go build -o bifrost ./cmd/bifrost

build-bif:
	CGO_ENABLED=0 go build -o bif ./cmd/bif

# Install the bif CLI. `go install` rather than a copy into a hand-picked
# directory: it already targets the one place Go tooling agrees is on PATH, so
# there is nothing to configure and nothing to keep in sync.
#
# Naming it `bif` rather than `ib` is what makes installing this safe while the
# port is unfinished. The Python `ib` (uv puts it in ~/.local/bin) still owns
# `promote` and `preview`; a Go binary called `ib` would shadow it on PATH and
# silently take those away, which you would discover during an incident. Two
# names, no collision, both work.
install:
	CGO_ENABLED=0 go install ./cmd/bif
	@echo "installed bif to $(GOBIN_DIR)"
	@if ! type -P bif >/dev/null 2>&1; then \
		echo "warning: 'bif' is not on PATH — add $(GOBIN_DIR) to it"; \
	fi

test:
	go test ./...

lint:
	go vet ./...
