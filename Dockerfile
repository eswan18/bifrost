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

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o bifrost ./cmd/bifrost

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
