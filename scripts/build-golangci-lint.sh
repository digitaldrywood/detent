#!/usr/bin/env bash
set -euo pipefail

version="$1"
heft_version="$2"
output="$3"
module=github.com/golangci/golangci-lint/v2
export GOWORK=off

temp_root="${TMPDIR:-${TMP:-${TEMP:-$PWD/tmp}}}"
mkdir -p "$temp_root"
build_dir="$(mktemp -d "$temp_root/detent-golangci-lint.XXXXXX")"
trap 'rm -rf "$build_dir"' EXIT

go mod download "$module@$version"
module_dir="$(go list -m -f '{{.Dir}}' "$module@$version")"
mkdir "$build_dir/source"
cp -R "$module_dir/." "$build_dir/source"
chmod -R u+w "$build_dir/source"

(
	cd "$build_dir/source"
	printf 'package main\n\nimport _ "github.com/digitaldrywood/heft/plugin"\n' > cmd/golangci-lint/plugins.go
	go get "github.com/digitaldrywood/heft/plugin@$heft_version"
	go build -buildvcs=false -ldflags "-s -w -X main.version=$version-heft-$heft_version -X main.date=module" \
		-o "$build_dir/golangci-lint-custom" ./cmd/golangci-lint
)

mkdir -p "$(dirname "$output")"
mv -f "$build_dir/golangci-lint-custom" "$output"
