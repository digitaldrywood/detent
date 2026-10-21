#!/usr/bin/env bash
set -euo pipefail

: "${CI_RELEASE_BRANCH:?release prebuild requires a release branch}"
: "${CI_DEVELOP_SHA:?release prebuild requires the validated commit}"
: "${GOOS:?}" "${GOARCH:?}"

series="${CI_RELEASE_BRANCH#release/}"
git fetch --tags --force origin
tag="$(git tag --points-at "$CI_DEVELOP_SHA" --list "$series.*" --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
if [ -z "$tag" ]; then
  latest="$(git tag --list "$series.*" --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
  if [[ "$latest" =~ ^v[0-9]+\.[0-9]+\.([0-9]+)$ ]]; then
    tag="$series.$((BASH_REMATCH[1] + 1))"
  else
    tag="$series.0"
  fi
fi
version="${tag#v}"
date="$(TZ=UTC git show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd "$CI_DEVELOP_SHA")"

env -u GOOS -u GOARCH make generate-docs
env -u GOOS -u GOARCH make source-metadata VERSION="$version" COMMIT="$CI_DEVELOP_SHA" DATE="$date"

binary=detent
if [ "$GOOS" = windows ]; then binary=detent.exe; fi
out="tmp/prebuilt/${GOOS}_${GOARCH}"
mkdir -p "$out"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version -X main.commit=$CI_DEVELOP_SHA -X main.date=$date" -o "$out/$binary" ./cmd/detent
printf '{"version":"%s","commit":"%s","date":"%s"}\n' "$version" "$CI_DEVELOP_SHA" "$date" > "$out/metadata.json"
echo "Prebuilt $out/$binary for $tag at $CI_DEVELOP_SHA"
