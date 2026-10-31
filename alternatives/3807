#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = build ] && [ -n "${DETENT_PREBUILT_DIR:-}" ]; then
  goos="${GOOS:-$(go env GOOS)}"
  goarch="${GOARCH:-$(go env GOARCH)}"
  binary=detent
  if [ "$goos" = windows ]; then binary=detent.exe; fi
  prebuilt="$DETENT_PREBUILT_DIR/${goos}_${goarch}/$binary"
  output=
  previous=
  for arg in "$@"; do
    if [ "$previous" = -o ]; then output="$arg"; fi
    previous="$arg"
  done
  if [ -f "$prebuilt" ] && [ -n "$output" ]; then
    mkdir -p "$(dirname "$output")"
    cp "$prebuilt" "$output"
    chmod 0755 "$output"
    echo "Using prebuilt $prebuilt" >&2
    exit 0
  fi
fi
exec go "$@"
