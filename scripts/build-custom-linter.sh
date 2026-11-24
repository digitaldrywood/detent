#!/usr/bin/env bash
set -euo pipefail

linter_version=${1:?linter version required}
heft_version=${2:?Heft version required}
output=${3:?output path required}
procs=${4:?process budget required}
[[ $output == /* ]]

output_dir=$(dirname "$output")
mkdir -p "$output_dir"
build_dir=$(mktemp -d "${TMPDIR:?job scratch required}/detent-linter.XXXXXX")
staged_binary=
trap 'rm -rf -- "$build_dir"; rm -f -- "$staged_binary"' EXIT
staged_binary=$(mktemp "$output_dir/.golangci-lint.XXXXXX")
cd "$build_dir"
export GOWORK=off

go mod init example.test/detent-linter
go mod edit -require="github.com/golangci/golangci-lint/v2@$linter_version" \
    -require="github.com/digitaldrywood/heft@$heft_version"
cat > main.go <<'EOF'
package main

import (
	"fmt"
	"os"
	"runtime"

	_ "github.com/digitaldrywood/heft/plugin"
	"github.com/golangci/golangci-lint/v2/pkg/commands"
	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
)

var version string

func main() {
	if err := commands.Execute(commands.BuildInfo{Version: version, GoVersion: runtime.Version()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitcodes.Failure)
	}
}
EOF
go build -mod=mod -buildvcs=false -p "$procs" -ldflags "-X main.version=${linter_version#v}" -o "$staged_binary" .
[[ $(go list -m -f '{{.Version}}' github.com/golangci/golangci-lint/v2) == "$linter_version" ]]
[[ $(go list -m -f '{{.Version}}' github.com/digitaldrywood/heft) == "$heft_version" ]]
mv -f -- "$staged_binary" "$output"
