VERSION ?= dev
GO ?= go
LDFLAGS = -ldflags "-X github.com/weaztoven/loadbear/internal/version.Version=$(VERSION) -X github.com/weaztoven/loadbear/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) -X github.com/weaztoven/loadbear/internal/version.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)"

.PHONY: build run test test-race cover bench lint fmt

ifeq ($(OS),Windows_NT)
EXE = .exe
endif

build:
	$(GO) build $(LDFLAGS) -trimpath -o loadbear$(EXE) ./cmd/loadbear
	$(GO) build $(LDFLAGS) -trimpath -o fakebackend$(EXE) ./cmd/fakebackend

run:
	$(GO) run $(LDFLAGS) ./cmd/loadbear --config examples/config.example.yaml

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

bench:
	$(GO) test -bench . -benchmem ./...

lint:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...
