# NetSonar Makefile

BINARY   := netsonar
BINDIR   := bin
PKG      := ./cmd/agent
MODULE   := netsonar

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE     ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS  := -s -w \
	-X 'main.version=$(VERSION)' \
	-X 'main.commit=$(COMMIT)' \
	-X 'main.date=$(DATE)'

.PHONY: all build test test-short test-race test-pbt lab-e2e lab-dev lab-dev-internet lab-dev-reload lab-dev-down lint fmt vet clean

all: fmt vet lint test build

build:
	mkdir -p $(BINDIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) $(PKG)

test:
	go test ./...

test-short:
	go test -short ./...

test-race:
	go test -race ./...

test-pbt:
	go test -run Property ./...

lab-e2e:
	./scripts/lab-e2e.sh

lab-dev:
	docker compose -f lab/dev-stack/docker-compose.yml up --build --force-recreate -d

lab-dev-internet:
	GOCACHE=$(CURDIR)/.cache/go-build go run ./tools/configmerge --base lab/dev-stack/config/netsonar.yaml --overlay lab/dev-stack/config/netsonar-internet.yaml --out lab/dev-stack/config/netsonar.with-internet.yaml
	NETSONAR_CONFIG=/config/netsonar.with-internet.yaml docker compose -f lab/dev-stack/docker-compose.yml up --build --force-recreate -d

lab-dev-reload:
	docker compose -f lab/dev-stack/docker-compose.yml kill -s SIGHUP netsonar

lab-dev-down:
	docker compose -f lab/dev-stack/docker-compose.yml down -v

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

vet:
	go vet ./...

clean:
	rm -rf $(BINDIR) .cache
