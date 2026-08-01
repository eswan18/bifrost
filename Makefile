SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := build

# Where `go install` puts binaries: GOBIN if set, else GOPATH/bin. This is the
# directory `make install` lands bif in, and the one that has to be on PATH.
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
# The name `bif` rather than `ib` is a leftover from the port: while it was in
# progress the Python `ib` still owned the commands not yet ported, and a Go
# binary called `ib` would have shadowed it on PATH and silently taken those
# away — the kind of thing you discover during an incident. The Python is
# retired now, but the name stays: renaming it would break every script and
# habit that has learned it, to save one character.
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
