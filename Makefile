# NetSonar Makefile

BINARY   := netsonar
BINDIR   := bin
PKG      := ./cmd/agent
MODULE   := netsonar
RELEASE_TMP := $(BINDIR)/release
GO       := GOCACHE=$(CURDIR)/.cache/go-build go

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_NO_V := $(VERSION:v%=%)
REVISION    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE  ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS  := -s -w \
	-X 'main.version=$(VERSION)' \
	-X 'main.revision=$(REVISION)' \
	-X 'main.buildDate=$(BUILD_DATE)'

.PHONY: help all tidy update-patch update-minor build build-release run test test-short test-race test-pbt lab-e2e lab-dev lab-dev-internet lab-dev-reload lab-dev-down lab-mv lab-mv-down lab-metrics-validation lab-metrics-validation-down lint fmt vet clean

help:
	@printf '%s\n' 'Available targets:'
	@printf '  %-29s %s\n' 'all' 'run fmt, vet, lint, test, and build'
	@printf '  %-29s %s\n' 'tidy' 'sync Go module dependencies'
	@printf '  %-29s %s\n' 'update-patch' 'update dependencies (patch only)'
	@printf '  %-29s %s\n' 'update-minor' 'update dependencies (minor + patch)'
	@printf '  %-29s %s\n' 'build' 'build the netsonar binary with version metadata'
	@printf '  %-29s %s\n' 'build-release' 'build release archives for Linux'
	@printf '  %-29s %s\n' 'run' 'run the CLI help'
	@printf '  %-29s %s\n' 'test' 'run all tests'
	@printf '  %-29s %s\n' 'test-short' 'run tests in short mode'
	@printf '  %-29s %s\n' 'test-race' 'run tests with the race detector'
	@printf '  %-29s %s\n' 'test-pbt' 'run property-based tests only'
	@printf '  %-29s %s\n' 'lint' 'run golangci-lint'
	@printf '  %-29s %s\n' 'fmt' 'format Go sources'
	@printf '  %-29s %s\n' 'vet' 'run go vet'
	@printf '  %-29s %s\n' 'lab-e2e' 'run end-to-end tests in Docker'
	@printf '  %-29s %s\n' 'lab-dev' 'start the local observability stack'
	@printf '  %-29s %s\n' 'lab-dev-internet' 'start dev-stack with public Internet smoke targets'
	@printf '  %-29s %s\n' 'lab-dev-reload' 'reload dev-stack agent config with SIGHUP'
	@printf '  %-29s %s\n' 'lab-dev-down' 'stop dev-stack and remove volumes'
	@printf '  %-29s %s\n' 'lab-mv' 'start the metrics validation lab'
	@printf '  %-29s %s\n' 'lab-mv-down' 'stop metrics validation lab and remove volumes'
	@printf '  %-29s %s\n' 'lab-metrics-validation' 'alias for lab-mv'
	@printf '  %-29s %s\n' 'lab-metrics-validation-down' 'alias for lab-mv-down'
	@printf '  %-29s %s\n' 'clean' 'remove build artifacts'

all: fmt vet lint test build

tidy:
	$(GO) mod tidy

update-patch:
	$(GO) get -u=patch ./...
	$(GO) mod tidy

update-minor:
	$(GO) get -u ./...
	$(GO) mod tidy

build:
	mkdir -p $(BINDIR)
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) $(PKG)

build-release:
	rm -rf $(RELEASE_TMP)
	mkdir -p $(RELEASE_TMP)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(RELEASE_TMP)/$(BINARY)_linux_amd64 $(PKG)
	mkdir -p $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64
	cp $(RELEASE_TMP)/$(BINARY)_linux_amd64 $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/$(BINARY)
	cp packaging/README.release.md $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/README.md
	cp LICENSE $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/LICENSE
	cp examples/config.yaml $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/config.example.yaml
	cp examples/systemd/netsonar.service $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/netsonar.service
	tar -C $(RELEASE_TMP) -czf $(BINDIR)/$(BINARY)_$(VERSION_NO_V)_linux_amd64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(RELEASE_TMP)/$(BINARY)_linux_arm64 $(PKG)
	mkdir -p $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64
	cp $(RELEASE_TMP)/$(BINARY)_linux_arm64 $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/$(BINARY)
	cp packaging/README.release.md $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/README.md
	cp LICENSE $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/LICENSE
	cp examples/config.yaml $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/config.example.yaml
	cp examples/systemd/netsonar.service $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/netsonar.service
	tar -C $(RELEASE_TMP) -czf $(BINDIR)/$(BINARY)_$(VERSION_NO_V)_linux_arm64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_arm64
	cd $(BINDIR) && sha256sum $(BINARY)_$(VERSION_NO_V)_linux_amd64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_arm64.tar.gz > checksums.txt

run:
	$(GO) run $(PKG) --help

test:
	$(GO) test ./...

test-short:
	$(GO) test -short ./...

test-race:
	$(GO) test -race ./...

test-pbt:
	$(GO) test -run Property ./...

lab-e2e:
	./scripts/lab-e2e.sh

lab-dev:
	docker compose -f lab/dev-stack/docker-compose.yml up --build --force-recreate -d

lab-dev-internet:
	$(GO) run ./tools/configmerge --base lab/dev-stack/config/netsonar.yaml --overlay lab/dev-stack/config/netsonar-internet.yaml --out lab/dev-stack/config/netsonar.with-internet.yaml
	NETSONAR_CONFIG=/config/netsonar.with-internet.yaml docker compose -f lab/dev-stack/docker-compose.yml up --build --force-recreate -d

lab-dev-reload:
	docker compose -f lab/dev-stack/docker-compose.yml kill -s SIGHUP netsonar

lab-dev-down:
	docker compose -f lab/dev-stack/docker-compose.yml down -v

lab-mv:
	docker compose -f lab/metrics-validation/docker-compose.yml up --build --force-recreate -d

lab-mv-down:
	docker compose -f lab/metrics-validation/docker-compose.yml down -v

lab-metrics-validation: lab-mv

lab-metrics-validation-down: lab-mv-down

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BINDIR) .cache
