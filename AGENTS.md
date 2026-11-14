# Repository Guidelines

## Project Structure & Module Organization

NetSonar is a Go network-probing agent. The executable entry point is `cmd/agent`; implementation lives under `internal/` (`config`, `probe`, `scheduler`, `metrics`, `doctor`, and `proxyurl`). Keep package tests beside their source as `*_test.go`. Documentation belongs in `docs/`, examples in `examples/`, and dashboards in `grafana/`. Docker-backed development, E2E, and metrics-validation environments live in `lab/`. Utilities belong in `tools/`; automation scripts belong in `scripts/`.

## Build, Test, and Development Commands

Use the Go version declared in `go.mod` (currently 1.27.1).

- `make build` creates the static binary at `bin/netsonar` with version metadata.
- `make run` runs the CLI help locally.
- `make test` runs all Go tests; `make test-short` skips long-running cases.
- `make test-race` enables the race detector; `make test-pbt` runs property tests.
- `go test -tags integration ./...` runs integration-tagged tests.
- `make lint`, `make vet`, and `make fmt` apply the repository’s static checks and formatting.
- `make all` runs formatting, vetting, linting, tests, and a build.
- `make lab-e2e` runs the Docker E2E suite; `make lab-dev` starts the local Prometheus/Grafana stack.

## Coding Style & Naming Conventions

Format Go with `gofmt -s`; do not hand-align source. Follow standard Go naming: exported identifiers use `CamelCase`, internal variables use concise `camelCase`, and package names are short lowercase words. Wrap errors with useful context and `%w`. Respect checks configured in `.golangci.yml`, including `gosec`, `errorlint`, `bodyclose`, and `noctx`. Run `go mod tidy` after dependency changes and commit both `go.mod` and `go.sum` updates.

## Testing Guidelines

Use the standard `testing` package; property tests use `gopter`. Name unit tests `TestFunction_Scenario` and property tests `TestProperty...`. Prefer table-driven cases and local test servers over external network dependencies. Add regression coverage with behavior changes. CI runs short, race, integration, E2E, and vulnerability checks; no numeric coverage threshold is defined.

## Commit & Pull Request Guidelines

Use Conventional Commit subjects seen in history, such as `feat: add probe option`, `fix: handle timeout`, or `docs: clarify deployment`. Keep commits focused. Pull requests should explain the motivation and behavior change, link relevant issues, list verification commands, and note configuration or metrics compatibility impacts. Include screenshots for Grafana/dashboard changes and update examples or docs when user-facing behavior changes.

## Security & Configuration

Never commit real credentials, private keys, or production target data. The key under `lab/e2e/fake-targets/` is test-only. Validate configuration changes against `examples/config.yaml`, and run `make vulncheck` when dependencies or network-facing code change.
