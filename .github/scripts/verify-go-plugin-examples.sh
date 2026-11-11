#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

# Root ./... patterns do not cross the examples' Go module boundaries.
while IFS= read -r manifest; do
  module_dir="${manifest%/go.mod}"
  output_dir="$build_dir/${module_dir#examples/plugin/}"
  mkdir -p "$output_dir"
  printf '\nChecking %s\n' "$module_dir"
  (
    cd "$module_dir"
    go test -mod=readonly -count=1 ./...
    go build -mod=readonly -buildmode=c-shared -o "$output_dir/plugin" .
  )
done < <(find examples/plugin -type f -name go.mod | LC_ALL=C sort)
