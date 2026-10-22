#!/usr/bin/env bash
set -euo pipefail

version="$1"
patch_path="$2"
output_path="$3"
repo_root="$(git rev-parse --show-toplevel)"
if [[ "$patch_path" != /* ]]; then
	patch_path="$repo_root/$patch_path"
fi
if [[ "$output_path" != /* ]]; then
	output_path="$repo_root/$output_path"
fi
module_cache="$(go env GOMODCACHE)"
module_path="$module_cache/github.com/securego/gosec/v2@$version"
temp_root="${TMPDIR:-${TMP:-${TEMP:-$repo_root/tmp}}}"
mkdir -p "$temp_root"
source_dir="$(mktemp -d "$temp_root/detent-gosec.XXXXXX")"

go mod download "github.com/securego/gosec/v2@$version"
mkdir -p "$source_dir/source" "$(dirname "$output_path")"
cp -R "$module_path/." "$source_dir/source"
chmod -R u+w "$source_dir/source"
patch -s -d "$source_dir/source" -p1 < "$patch_path"
(
	cd "$source_dir/source"
	go build -o "$output_path" ./cmd/gosec
)
