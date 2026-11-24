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
	go-linter)
		version="$2"
		heft_version="$3"
		binary="$4"
		module=github.com/golangci/golangci-lint/v2
		temp_root="${TMPDIR:-${TMP:-${TEMP:-$PWD/tmp}}}"
		mkdir -p "$temp_root" "$(dirname "$binary")"
		build_dir="$(mktemp -d "$temp_root/detent-linter.XXXXXX")"
		trap 'rm -rf -- "$build_dir"' EXIT
		export GOWORK=off
		go mod download "$module@$version"
		cp "$(go env GOMODCACHE)/$module@$version/cmd/golangci-lint/main.go" "$build_dir/"
		cat > "$build_dir/plugins.go" <<'EOF'
package main

import _ "github.com/digitaldrywood/heft/plugin"
EOF
		(
			cd "$build_dir"
			go mod init example.com/detent-linter
			go mod edit -require="$module@$version" -require="github.com/digitaldrywood/heft@$heft_version"
			go build -mod=mod -p "${TEST_PROCS:-4}" -ldflags "-s -w -X main.version=$version-custom-gcl -X main.date=unknown" -o golangci-lint-custom .
		)
		mv "$build_dir/golangci-lint-custom" "$binary"
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
