#!/usr/bin/env sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
compose="docker compose -f lab/e2e/docker-compose.yml"

cleanup() {
	cd "$repo_root"
	$compose down -v --remove-orphans
}

trap cleanup EXIT INT TERM

cd "$repo_root"
$compose up --build --abort-on-container-exit --exit-code-from test-runner test-runner
