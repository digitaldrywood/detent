#!/usr/bin/env bash
set -euo pipefail

binary=${1:?output path required}
version=${2:?golangci-lint version required}
heft_version=${3:?heft version required}
procs=${4:?process budget required}
linter_module=github.com/golangci/golangci-lint/v2
heft_module=github.com/digitaldrywood/heft
export GOWORK=off

build_dir=$(mktemp -d "${TMPDIR:-/tmp}/detent-custom-linter.XXXXXX")
trap 'rm -rf "$build_dir"' EXIT
mkdir -p "$(dirname "$binary")"

cd "$build_dir"
go mod init detent.tools/golangci-lint-custom
go mod edit -require="$linter_module@$version" -require="$heft_module@$heft_version"
source_dir=$(go mod download -json "$linter_module@$version" | python3 -c 'import json, sys; print(json.load(sys.stdin)["Dir"])')
cp "$source_dir/cmd/golangci-lint/main.go" .
printf 'package main\n\nimport _ "%s/plugin"\n' "$heft_module" > plugins.go
go build -p "$procs" -mod=mod -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${version#v} -X main.date=(unknown)" -o "$binary" .
