#!/usr/bin/env bash
set -euo pipefail

tag="${1:?usage: release-use-prebuilt.sh <tag>}"
version="${tag#v}"
date="$(TZ=UTC git show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd "$GITHUB_SHA")"
dir="$PWD/tmp/prebuilt"

target_url="$(gh api "repos/$GITHUB_REPOSITORY/commits/$GITHUB_SHA/status" --jq '[.statuses[] | select(.context == "scheduled-full-ci" and .state == "success")][0].target_url // empty')"
run_id="${target_url##*/runs/}"
run_id="${run_id%%/*}"
if ! [[ "$run_id" =~ ^[0-9]+$ ]]; then
  echo "No validated suite run recorded for $GITHUB_SHA; building binaries in this job."
  exit 0
fi

rm -rf "$dir"
if ! gh run download "$run_id" --repo "$GITHUB_REPOSITORY" --pattern 'release-prebuilt-*' --dir "$dir.download"; then
  echo "Prebuilt binaries from run $run_id are unavailable; building binaries in this job."
  exit 0
fi
mkdir -p "$dir"
for artifact in "$dir.download"/release-prebuilt-*; do
  cp -R "$artifact"/. "$dir"/
done
rm -rf "$dir.download"

for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64 windows_amd64 windows_arm64; do
  binary=detent
  if [[ "$target" == windows_* ]]; then binary=detent.exe; fi
  if [ ! -f "$dir/$target/$binary" ] || ! jq -e --arg version "$version" --arg commit "$GITHUB_SHA" --arg date "$date" '.version == $version and .commit == $commit and .date == $date' "$dir/$target/metadata.json" > /dev/null 2>&1; then
    echo "Prebuilt $target does not match $tag at $GITHUB_SHA; building binaries in this job."
    rm -rf "$dir"
    exit 0
  fi
done

echo "DETENT_PREBUILT_DIR=$dir" >> "$GITHUB_ENV"
echo "Using binaries prebuilt by suite run $run_id for $tag."
