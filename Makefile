# NetSonar Makefile

BINARY   := netsonar
BINDIR   := bin
PKG      := ./cmd/agent
MODULE   := netsonar

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE     ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS  := -s -w \
	-X 'main.version=$(VERSION)'

.PHONY: all build test test-short test-race test-pbt lint fmt vet clean

all: fmt vet lint test build

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) $(PKG)

test:
	go test ./...

test-short:
	go test -short ./...

test-race:
	go test -race ./...

test-pbt:
	go test -run Property ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

vet:
	go vet ./...

clean:
	rm -rf $(BINDIR)
