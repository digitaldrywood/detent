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

install_go_tool() {
	local binary="$1" package="$2" version="$3" toolchain="$4" available
	if matches_go_tool "$binary" "$package" "$version" "$toolchain"; then
		return
	fi
	mkdir -p "$(dirname "$binary")"
	available="$(command -v "${binary##*/}" || true)"
	if [[ "$available" != "$binary" ]] && matches_go_tool "$available" "$package" "$version" "$toolchain"; then
		cp "$available" "$binary"
	else
		printf 'Installing %s@%s\n' "$package" "$version"
		GOTOOLCHAIN="${toolchain:-${GOTOOLCHAIN:-auto}}" GOBIN="$(dirname "$binary")" go install -p "${TEST_PROCS:-4}" "$package@$version"
	fi
}

build_custom_linter() (
	local version="$1" heft_version="$2" binary="$3"
	local release='^v[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.+-]+)?$'
	local module=github.com/golangci/golangci-lint/v2
	local temp_root="${TMPDIR:-${TMP:-${TEMP:-$PWD/tmp}}}" build_dir
	if [[ ! "$version" =~ $release || ! "$heft_version" =~ $release ]]; then
		printf 'Custom linter requires pinned golangci-lint and Heft versions\n' >&2
		return 1
	fi
	if [[ "$binary" != /* ]]; then
		binary="$PWD/$binary"
	fi
	mkdir -p "$temp_root" "$(dirname "$binary")"
	temp_root="$(cd "$temp_root" && pwd)"
	build_dir="$(mktemp -d "$temp_root/detent-linter.XXXXXX")"
	trap 'rm -rf -- "$build_dir"' EXIT
	cd "$build_dir"
	export GOWORK=off
	go mod init example.com/detent-linter
	go mod download "$module@$version"
	cp "$(go env GOMODCACHE)/$module@$version/cmd/golangci-lint/main.go" .
	cat > plugins.go <<'EOF'
package main

import _ "github.com/digitaldrywood/heft/plugin"
EOF
	go mod edit -require="$module@$version" -require="github.com/digitaldrywood/heft@$heft_version"
	go build -mod=mod -p "${TEST_PROCS:-4}" -ldflags "-s -w -X main.version=$version-custom-gcl -X main.date=unknown" -o golangci-lint-custom .
	mv golangci-lint-custom "$binary"
)

run_linter() {
	local version="$1" heft_version="$2"
	shift 2
	if [[ "$#" == 5 && "$1" == custom && "$2" == --destination && "$4" == --name && "$5" == golangci-lint-custom ]]; then
		build_custom_linter "$version" "$heft_version" "$3/$5"
		return
	fi
	exec "$0.upstream" "$@"
}

install_linter_launcher() (
	local binary="$1" version="$2" heft_version="$3" launcher
	launcher="$(mktemp "$binary.XXXXXX")"
	trap 'rm -f -- "$launcher"' EXIT
	{
		printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail'
		declare -f build_custom_linter run_linter
		printf 'run_linter %q %q "$@"\n' "$version" "$heft_version"
	} > "$launcher"
	chmod 0755 "$launcher"
	mv -f -- "$launcher" "$binary"
)

read_heft_version() {
	local config=.custom-gcl.yml contents=""
	if [[ ! -f "$config" || -L "$config" ]]; then
		printf 'Custom linter configuration must be a regular file\n' >&2
		return 1
	fi
	if IFS= LC_ALL=C read -r -d '' -n 4097 contents < "$config"; then
		printf 'Custom linter configuration must be at most 4096 bytes and contain no NUL bytes\n' >&2
		return 1
	fi
	awk '/digitaldrywood\/heft$/ { found = 1 } found && $1 == "version:" { print $2; exit }' <<< "$contents"
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
		if [[ "$package" == github.com/golangci/golangci-lint/v2/cmd/golangci-lint && "${GIT_ALLOW_PROTOCOL-unrestricted}" == "" && "$binary" == "$PWD"/tmp/tools/golangci-lint/* && ( -e .custom-gcl.yml || -L .custom-gcl.yml ) ]]; then
			heft_version="$(read_heft_version)"
			if ! matches_go_tool "$binary.upstream" "$package" "$version" "$toolchain"; then
				install_go_tool "$binary" "$package" "$version" "$toolchain"
				mv "$binary" "$binary.upstream"
			fi
			install_linter_launcher "$binary" "$version" "$heft_version"
		else
			install_go_tool "$binary" "$package" "$version" "$toolchain"
		fi
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
