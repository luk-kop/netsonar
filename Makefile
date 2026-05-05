# NetSonar Makefile

BINARY   := netsonar
BINDIR   := bin
PKG      := ./cmd/agent
MODULE   := netsonar
RELEASE_TMP := $(BINDIR)/release

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_NO_V := $(VERSION:v%=%)
REVISION    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE  ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS  := -s -w \
	-X 'main.version=$(VERSION)' \
	-X 'main.revision=$(REVISION)' \
	-X 'main.buildDate=$(BUILD_DATE)'

.PHONY: all build build-release test test-short test-race test-pbt lab-e2e lab-dev lab-dev-internet lab-dev-reload lab-dev-down lab-mv lab-mv-down lab-metrics-validation lab-metrics-validation-down lint fmt vet clean

all: fmt vet lint test build

build:
	mkdir -p $(BINDIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/$(BINARY) $(PKG)

build-release:
	rm -rf $(RELEASE_TMP)
	mkdir -p $(RELEASE_TMP)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(RELEASE_TMP)/$(BINARY)_linux_amd64 $(PKG)
	mkdir -p $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64
	cp $(RELEASE_TMP)/$(BINARY)_linux_amd64 $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/$(BINARY)
	cp packaging/README.release.md $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/README.md
	cp LICENSE $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/LICENSE
	cp examples/config.yaml $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/config.example.yaml
	cp examples/systemd/netsonar.service $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_amd64/netsonar.service
	tar -C $(RELEASE_TMP) -czf $(BINDIR)/$(BINARY)_$(VERSION_NO_V)_linux_amd64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(RELEASE_TMP)/$(BINARY)_linux_arm64 $(PKG)
	mkdir -p $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64
	cp $(RELEASE_TMP)/$(BINARY)_linux_arm64 $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/$(BINARY)
	cp packaging/README.release.md $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/README.md
	cp LICENSE $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/LICENSE
	cp examples/config.yaml $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/config.example.yaml
	cp examples/systemd/netsonar.service $(RELEASE_TMP)/$(BINARY)_$(VERSION_NO_V)_linux_arm64/netsonar.service
	tar -C $(RELEASE_TMP) -czf $(BINDIR)/$(BINARY)_$(VERSION_NO_V)_linux_arm64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_arm64
	cd $(BINDIR) && sha256sum $(BINARY)_$(VERSION_NO_V)_linux_amd64.tar.gz $(BINARY)_$(VERSION_NO_V)_linux_arm64.tar.gz > checksums.txt

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
	go vet ./...

clean:
	rm -rf $(BINDIR) .cache
