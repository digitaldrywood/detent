#!/usr/bin/env bash
set -euo pipefail

lint_version=${1:?golangci-lint version required}
heft_version=${2:?Heft version required}
output=${3:?output path required}
procs=${4:?process budget required}
if [[ "$output" != /* ]]; then
	output="$PWD/$output"
fi
temp_root=${TMPDIR:-${TMP:-${TEMP:-"$PWD/tmp"}}}
mkdir -p "$temp_root" "$(dirname "$output")"
scratch=$(mktemp -d "$temp_root/detent-custom-linter.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
cd "$scratch"
export GOWORK=off

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

go mod init detent.example/custom-linter
go get "github.com/golangci/golangci-lint/v2@$lint_version" "github.com/digitaldrywood/heft@$heft_version"
go build -mod=mod -p "$procs" -buildvcs=false -ldflags "-s -w -X main.version=${lint_version#v}" -o golangci-lint-custom .
mv -f golangci-lint-custom "$output"
