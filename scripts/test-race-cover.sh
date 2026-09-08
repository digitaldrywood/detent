#!/usr/bin/env bash
set -euo pipefail

mkdir -p tmp
profile_dir="$(mktemp -d "${TMPDIR:-$PWD/tmp}/detent-race-cover.XXXXXX")"
publish_path=
trap 'rm -rf "$profile_dir"; if [ -n "$publish_path" ]; then rm -f "$publish_path"; fi' EXIT

selection='{{.ImportPath}} {{.GoFiles}} {{.CgoFiles}} {{.TestGoFiles}} {{.XTestGoFiles}}'
go list -f "$selection" ./internal/hubserver > "$profile_dir/ordinary-inputs"
go list -race -f "$selection" ./internal/hubserver > "$profile_dir/race-inputs"
if ! cmp -s "$profile_dir/ordinary-inputs" "$profile_dir/race-inputs"; then
    echo "Hub race and coverage source selections differ; combined coverage is unsafe" >&2
    exit 1
fi

env -u DETENT_API_TOKEN go run ./tools/testgate -race -coverprofile "$profile_dir/hub.out" -parallel "$1" -timeout "$2" -output tmp/hub-race-evidence ./internal/hubserver
go list ./... > "$profile_dir/packages"
packages=()
while IFS= read -r package; do
    if [ "$package" != github.com/digitaldrywood/detent/internal/hubserver ]; then
        packages+=("$package")
    fi
done < "$profile_dir/packages"
if [ "${#packages[@]}" -eq 0 ]; then
    echo "No remaining test packages" >&2
    exit 1
fi
env -u DETENT_API_TOKEN go test -race "${packages[@]}"
env -u DETENT_API_TOKEN go test -coverprofile="$profile_dir/rest.out" "${packages[@]}"
go run ./tools/covermerge "$profile_dir/hub.out" "$profile_dir/rest.out" > "$profile_dir/merged.out"
publish_path="$(mktemp "$3.XXXXXX")"
cat "$profile_dir/merged.out" > "$publish_path"
mv "$publish_path" "$3"
