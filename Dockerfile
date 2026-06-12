# Tailwind CSS build stage
FROM node:22-alpine AS tailwind

WORKDIR /build

COPY package.json package-lock.json ./
RUN npm ci

COPY static/input.css ./static/
COPY templates ./templates

RUN npx @tailwindcss/cli -i static/input.css -o static/style.css --minify

# Go build stage
FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

# Pre-compile the heavy dependencies (k8s client-go and friends) so they land
# in the Go build cache in a layer that only invalidates when go.mod/go.sum
# change. Env and flags must match the final go build exactly, or the cache
# entries won't be reused.
RUN CGO_ENABLED=0 GOOS=linux go build \
    k8s.io/client-go/kubernetes \
    k8s.io/client-go/dynamic \
    k8s.io/client-go/tools/clientcmd \
    github.com/coreos/go-oidc/v3/oidc \
    golang.org/x/oauth2

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o bifrost ./cmd/bifrost

# Runtime
FROM alpine:latest

RUN apk --no-cache add ca-certificates \
    && adduser -D -u 1000 appuser

WORKDIR /app

COPY --from=builder /build/bifrost .
COPY --from=builder /build/templates ./templates
COPY --from=tailwind /build/static ./static

USER appuser

EXPOSE 8080

ENV HTTP_ADDRESS=:8080
ENV TEMPLATES_DIR=/app/templates
ENV STATIC_DIR=/app/static

CMD ["./bifrost"]
