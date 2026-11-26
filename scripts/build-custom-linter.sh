#!/usr/bin/env bash
set -euo pipefail

version="${1:?golangci-lint module version required}"
heft_version="${2:?Heft module version required}"
output="${3:?output path required}"
case "$output" in
    /*) ;;
    *) output="$PWD/$output" ;;
esac

module=github.com/golangci/golangci-lint/v2
module_path="$(go env GOMODCACHE)/$module@$version"
scratch_root="${TMPDIR:-${TMP:-${TEMP:-$PWD/tmp}}}"
mkdir -p "$scratch_root"
scratch="$(mktemp -d "$scratch_root/detent-custom-linter.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

(
    cd "$scratch"
    export GOWORK=off
    go mod download "$module@$version"
    mkdir source
    cp -R "$module_path/." source
    chmod -R u+w source
    cd source
    printf 'package main\n\nimport _ "github.com/digitaldrywood/heft/plugin"\n' > cmd/golangci-lint/heft.go
    go get "github.com/digitaldrywood/heft@$heft_version"
    go build -mod=mod -buildvcs=false -ldflags "-s -w -X main.version=$version-heft-$heft_version -X main.date=module" -o "$scratch/golangci-lint-custom" ./cmd/golangci-lint
)
mkdir -p "$(dirname "$output")"
mv "$scratch/golangci-lint-custom" "$output"
