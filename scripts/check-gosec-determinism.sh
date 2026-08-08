#!/usr/bin/env bash
set -euo pipefail

binary_path="$1"
runs="$2"
if [[ ! "$runs" =~ ^[1-9][0-9]*$ ]]; then
	printf 'gosec determinism runs must be a positive integer, got %s\n' "$runs" >&2
	exit 1
fi
repo_root="$(git rev-parse --show-toplevel)"
if [[ "$binary_path" != /* ]]; then
	binary_path="$repo_root/$binary_path"
fi
temp_root="${TMPDIR:-${TMP:-${TEMP:-$repo_root/tmp}}}"
mkdir -p "$temp_root"
fixture_dir="$(mktemp -d "$temp_root/detent-gosec-fixture.XXXXXX")"

printf 'module detent.example/gosecfixture\n\ngo 1.26\n' > "$fixture_dir/go.mod"
{
	printf 'package main\n\n'
	printf 'import "net/http"\n\n'
	printf 'func writeResponse(writer http.ResponseWriter, value string) {\n'
	printf '\t_, _ = writer.Write([]byte(value))\n'
	printf '}\n\n'
	printf 'func aTainted(writer http.ResponseWriter, request *http.Request) {\n'
	printf '\twriteResponse(writer, request.URL.Query().Get("value"))\n'
	printf '}\n\n'
	for ((caller = 0; caller < 8192; caller++)); do
		printf 'func filler%s() { writeResponse(nil, "static") }\n' "$caller"
	done
	printf '\nfunc main() {\n'
	printf '\thttp.HandleFunc("/", aTainted)\n'
	printf '}\n'
} > "$fixture_dir/main.go"

for ((run = 1; run <= runs; run++)); do
	output="$("$binary_path" -fmt=json -no-fail -include=G705 -exclude-generated "$fixture_dir" 2>/dev/null)"
	finding_count="$(printf '%s\n' "$output" | awk '/"rule_id": "G705"/ { count++ } END { print count + 0 }')"
	if [[ "$finding_count" != "1" ]]; then
		printf 'gosec determinism run %s: expected 1 G705 finding, got %s\n' "$run" "$finding_count" >&2
		exit 1
	fi
done

printf 'gosec G705 finding was stable across %s runs\n' "$runs"
