#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

go version
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

go build -o "$build_dir/cli-proxy-api" ./cmd/server
"$build_dir/cli-proxy-api" --help > "$build_dir/help.txt" 2>&1

bash .github/scripts/verify-go-plugin-examples.sh

go test -count=1 ./...
go test -race -count=1 \
  ./internal/runtime/executor/helps \
  ./internal/api \
  ./sdk/api/handlers/openai \
  ./sdk/cliproxy/auth \
  -run 'TestEnrich|TestManagementV8|TestServerResponsesWebsocketReauthorizesEachTurn|TestStreamingPeek|TestFallback'
