#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
install_dir="${SYMPHONY_INSTALL_DIR:-"$HOME/.local/bin"}"
state_dir="${SYMPHONY_STATE_DIR:-"$HOME/.symphony"}"
lock_path="${SYMPHONY_INSTALL_LOCK:-"$state_dir/install.lock"}"
target="$install_dir/symphony"
source_binary="${SYMPHONY_INSTALL_SOURCE:-}"

mkdir -p "$install_dir" "$state_dir"

if ! (set -C; : > "$lock_path") 2>/dev/null; then
	echo "Symphony is already installed on this host: $lock_path" >&2
	exit 1
fi

cleanup_lock=true
cleanup() {
	if [ "$cleanup_lock" = true ]; then
		rm -f "$lock_path"
	fi
}
trap cleanup EXIT

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/symphony-install.XXXXXX")"
cleanup_tmp() {
	rm -rf "$tmp_dir"
}
trap 'cleanup_tmp; cleanup' EXIT

if [ -n "$source_binary" ]; then
	cp "$source_binary" "$tmp_dir/symphony"
else
	build_version="$(git -C "$repo_root" describe --tags --always 2>/dev/null || echo dev)"
	build_commit="$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo none)"
	build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	ldflags="-X main.version=$build_version -X main.commit=$build_commit -X main.date=$build_date"
	(cd "$repo_root" && go build -ldflags "$ldflags" -o "$tmp_dir/symphony" ./cmd/symphony)
fi

install -m 0755 "$tmp_dir/symphony" "$target"

{
	printf 'binary=%s\n' "$target"
	printf 'installed_at=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
} > "$lock_path"

cleanup_lock=false
echo "Installed Symphony at $target"
