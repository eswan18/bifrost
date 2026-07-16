SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := build

.PHONY: run build test lint

# static/style.css is hand-written and committed — there is no CSS build step.
run:
	go run ./cmd/bifrost

build:
	CGO_ENABLED=0 go build -o bifrost ./cmd/bifrost

test:
	go test ./...

lint:
	go vet ./...
