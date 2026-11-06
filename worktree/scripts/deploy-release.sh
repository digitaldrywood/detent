#!/usr/bin/env bash
set -euo pipefail

environment="${1:?environment required}"
commit="${2:?commit required}"
version="${3:?release version required}"
mode="${4:-standard}"
scratch="${TMPDIR:-${TMP:-${TEMP:?release deployment requires provided scratch}}}"
source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "$#" -gt 4 ]; then exit 2; fi
[[ "$commit" =~ ^[a-f0-9]{40}$ ]] || exit 2
case "$environment" in
  staging) host=staging.cloud.detent.build; alias=staging.hub.detent.build ;;
  production) host=cloud.detent.build; alias=hub.detent.build ;;
  *) exit 2 ;;
esac
case "$mode" in
  standard) ;;
  postgres-cutover)
    [[ "$environment" = production ]] || { echo "PostgreSQL cutover is production-only" >&2; exit 2; }
    [[ "$(git -C "$source_root" rev-parse --verify 'HEAD^{commit}')" = "$commit" ]] || { echo "cutover source does not match the exact release commit" >&2; exit 1; }
    git -C "$source_root" diff --quiet "$commit" -- deploy/hub/cutover-postgres.sh || { echo "cutover receiver differs from the exact release source" >&2; exit 1; }
    ;;
  *) exit 2 ;;
esac
case "$environment:$version" in
  staging:develop-[a-f0-9]*) [[ "$version" =~ ^develop-[a-f0-9]{7,40}$ ]] || exit 2 ;;
  *)
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-op\.(g?[a-f0-9]{12}|r\.[1-9][0-9]*\.g[a-f0-9]{12}))?$ ]] || exit 2
    if [[ "$version" == *-op.* ]]; then
      suffix="${version##*.}"
      test "${suffix#g}" = "${commit:0:12}"
    fi
    ;;
esac
test -n "${SSH_KEY:-}" || { echo "${environment} SSH key is required" >&2; exit 1; }
test -n "${KNOWN_HOSTS:-}" || { echo "${environment} pinned SSH host key is required" >&2; exit 1; }
ssh_dir="$(mktemp -d "$scratch/release-ssh.XXXXXX")"
trap 'rm -rf "$ssh_dir"' EXIT
printf '%s\n' "$SSH_KEY" > "$ssh_dir/id"
printf '%s\n' "$KNOWN_HOSTS" > "$ssh_dir/known_hosts"
chmod 600 "$ssh_dir/id" "$ssh_dir/known_hosts"
chmod 700 "$scratch/release-hub/detent"
identity="$("$scratch/release-hub/detent" version --format json)"
jq -e --arg version "${version#v}" --arg commit "$commit" '.version == $version and .commit == $commit' <<< "$identity" > /dev/null
ssh_args=(
  -i "$ssh_dir/id"
  -o IdentitiesOnly=yes
  -o HostKeyAlias="$alias"
  -o UserKnownHostsFile="$ssh_dir/known_hosts"
  -o StrictHostKeyChecking=yes
  -o BatchMode=yes
  -o ConnectTimeout=20
  "apprunner@$host"
)
if [ "$mode" = postgres-cutover ]; then
  [ -z "$(git -C "$source_root" status --porcelain --untracked-files=all)" ] || { echo "cutover source checkout is not clean" >&2; exit 1; }
  source_bundle_dir="$(mktemp -d "$scratch/release-source.XXXXXX")"
  payload="$(mktemp "$scratch/release-postgres-cutover.XXXXXX")"
  trap 'rm -rf "$ssh_dir" "$source_bundle_dir"; rm -f "$payload"' EXIT
  git -C "$source_bundle_dir" init -q
  git -C "$source_bundle_dir" fetch --no-tags --depth=1 "$source_root" "$commit"
  git -C "$source_bundle_dir" checkout --quiet --detach FETCH_HEAD
  [ "$(git -C "$source_bundle_dir" rev-parse --verify 'HEAD^{commit}')" = "$commit" ] || { echo "cutover source bundle commit does not match the release" >&2; exit 1; }
  git -C "$source_bundle_dir" bundle create "$source_bundle_dir/source.bundle" HEAD
  [ "$(git -C "$source_bundle_dir" bundle list-heads "$source_bundle_dir/source.bundle")" = "$commit HEAD" ] || { echo "cutover source bundle is not bound to the release commit" >&2; exit 1; }
  source_bundle_size="$(python3 -c 'import os,sys; print(os.path.getsize(sys.argv[1]))' "$source_bundle_dir/source.bundle")"
  [ "$source_bundle_size" -le 419430400 ] || { echo "cutover source bundle exceeds the upload limit" >&2; exit 1; }
  COPYFILE_DISABLE=1 tar -cf "$payload" -C "$scratch/release-hub" detent -C "$source_root/deploy/hub" cutover-postgres.sh -C "$source_bundle_dir" source.bundle
  remote_command="cutover-postgres $commit"
  ssh "${ssh_args[@]}" "$remote_command" < "$payload"
else
  ssh "${ssh_args[@]}" "deploy $commit" < "$scratch/release-hub/detent"
fi
