SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := build

# Where `go install` puts binaries: GOBIN if set, else GOPATH/bin. This is the
# directory `make install` lands ib in, and the one that has to be on PATH.
GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR := $(shell go env GOPATH)/bin
endif

.PHONY: run build build-ib install test lint

# static/style.css is hand-written and committed — there is no CSS build step.
run:
	go run ./cmd/bifrost

build:
	CGO_ENABLED=0 go build -o bifrost ./cmd/bifrost

build-ib:
	CGO_ENABLED=0 go build -o ib ./cmd/ib

# Install the ib CLI. `go install` rather than a copy into a hand-picked
# directory: it already targets the one place Go tooling agrees is on PATH, so
# there is nothing to configure and nothing to keep in sync.
#
# The PATH check afterwards is not decoration. Until promote and preview are
# ported, this binary is a strict subset of the Python `ib` — and if the
# Python one is also installed (uv puts it in ~/.local/bin), whichever comes
# first on PATH wins silently. Finding that out during an incident, with
# `ib promote bifrost` answering "not ported yet", is the failure this prints
# to avoid.
install:
	CGO_ENABLED=0 go install ./cmd/ib
	@echo "installed ib to $(GOBIN_DIR)"
	@found=$$(type -aP ib 2>/dev/null || true); \
	first=$$(printf '%s' "$$found" | head -1); \
	shadowed=$$(printf '%s' "$$found" | tail -n +2 | tr '\n' ' '); \
	if [ -z "$$found" ]; then \
		echo "warning: 'ib' is not on PATH — add $(GOBIN_DIR) to it"; \
	elif [ "$$first" != "$(GOBIN_DIR)/ib" ]; then \
		echo "note: $$first comes first on PATH, so 'ib' still runs that one, not this build"; \
	elif [ -n "$$shadowed" ]; then \
		echo "warning: this now shadows $$shadowed"; \
		echo "         'ib promote' and 'ib preview' are NOT ported yet — run that one directly for those"; \
	fi

test:
	go test ./...

lint:
	go vet ./...
