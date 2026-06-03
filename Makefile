SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
TEMPLATES := $(wildcard templates/*.html)

.DEFAULT_GOAL := build

css: static/style.css

static/style.css: static/input.css $(TEMPLATES)
	npx @tailwindcss/cli -i static/input.css -o static/style.css --minify

css-watch:
	npx @tailwindcss/cli -i static/input.css -o static/style.css --watch

run: css
	go run ./cmd/bifrost

build: css
	CGO_ENABLED=0 go build -o bifrost ./cmd/bifrost

test:
	go test ./...

lint:
	go vet ./...
