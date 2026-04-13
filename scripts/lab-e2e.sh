#!/usr/bin/env sh
set -eu

compose="docker compose -f lab/e2e/docker-compose.yml"

cleanup() {
	$compose down -v --remove-orphans
}

trap cleanup EXIT INT TERM

$compose up --build --abort-on-container-exit --exit-code-from test-runner test-runner
