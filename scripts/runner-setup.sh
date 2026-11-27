#!/usr/bin/env bash
set -euo pipefail

matches_go_tool() {
	local binary="$1" package="$2" version="$3" toolchain="$4"
	[[ -x "$binary" ]] || return 1
	go version -m "$binary" 2>/dev/null | awk -v package="$package" -v version="$version" -v toolchain="$toolchain" '
		NR == 1 { compiler = $NF }
		$1 == "path" { path = $2 }
		$1 == "mod" { module_version = $3 }
		$1 == "=>" { replaced = 1 }
		END { exit !(path == package && module_version == version && !replaced && (toolchain == "" || compiler == toolchain)) }
	'
}

case "${1:-}" in
	go-tool)
		package="$2"
		version="$3"
		binary="$4"
		toolchain="${5:-}"
		if [[ "$binary" != */* ]]; then
			install_dir="$(go env GOBIN)"
			install_dir="${install_dir:-$(go env GOPATH)/bin}"
			binary="$install_dir/$binary"
		fi
		if matches_go_tool "$binary" "$package" "$version" "$toolchain"; then
			exit 0
		fi
		mkdir -p "$(dirname "$binary")"
		available="$(command -v "${binary##*/}" || true)"
		if [[ "$available" != "$binary" ]] && matches_go_tool "$available" "$package" "$version" "$toolchain"; then
			cp "$available" "$binary"
		else
			printf 'Installing %s@%s\n' "$package" "$version"
			GOTOOLCHAIN="${toolchain:-${GOTOOLCHAIN:-auto}}" GOBIN="$(dirname "$binary")" go install -p "${TEST_PROCS:-4}" "$package@$version"
		fi
		;;
	heft-linter)
		lint_version="${2:?golangci-lint version required}"
		heft_version="${3:?heft version required}"
		binary="${4:?linter output path required}"
		module=github.com/golangci/golangci-lint/v2
		export GOWORK=off
		temp_root="${TMPDIR:-${TMP:-${TEMP:-$PWD/tmp}}}"
		mkdir -p "$temp_root" "$(dirname "$binary")"
		build_dir="$(mktemp -d "$temp_root/detent-heft-linter.XXXXXX")"
		build_output=""
		trap 'rm -rf "$build_dir"; if [[ -n "$build_output" ]]; then rm -f "$build_output"; fi' EXIT
		printf 'Building golangci-lint %s with Heft %s from Go modules\n' "$lint_version" "$heft_version"
		(
			cd "$build_dir"
			go mod init detent-linter
			go mod edit -require "$module@$lint_version" -require "github.com/digitaldrywood/heft@$heft_version"
			go mod download "$module"
			cp "$(go env GOMODCACHE)/$module@$lint_version/cmd/golangci-lint/main.go" main.go
			printf 'package main\n\nimport _ "github.com/digitaldrywood/heft/plugin"\n' > plugins.go
			go build -p "${TEST_PROCS:-4}" -mod=mod -trimpath \
				-ldflags "-s -w -X main.version=$lint_version-custom-heft-$heft_version -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
				-o golangci-lint-custom .
		)
		build_output="$(mktemp "$binary.XXXXXX")"
		install -m 0755 "$build_dir/golangci-lint-custom" "$build_output"
		mv -f "$build_output" "$binary"
		;;
	node-deps)
		cd "$2"
		[[ -f package.json ]] || exit 0
		digest="$(node -e '
			const fs = require("node:fs");
			const hash = require("node:crypto").createHash("sha256");
			for (const file of ["package.json", "package-lock.json"]) hash.update(fs.readFileSync(file));
			hash.update([process.version, process.platform, process.arch].join("\0"));
			process.stdout.write(hash.digest("hex"));
		')"
		stamp=node_modules/.detent-runner-setup
		if [[ -f "$stamp" && "$(cat "$stamp")" == "$digest" ]]; then
			exit 0
		fi
		npm ci --include=dev --no-audit --no-fund
		printf '%s\n' "$digest" > "$stamp"
		;;
	"")
		exec make setup GOLANGCI_LINT_DIR="$(go env GOPATH)/bin" GOBIN="$(go env GOPATH)/bin"
		;;
	*)
		printf 'Unknown setup action: %s\n' "$1" >&2
		exit 1
		;;
esac
