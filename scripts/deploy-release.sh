#!/usr/bin/env bash
set -euo pipefail

environment="${1:?environment required}"
commit="${2:?commit required}"
version="${3:?release version required}"
scratch="${TMPDIR:-${TMP:-${TEMP:?release deployment requires provided scratch}}}"
case "$environment" in
  staging) host=staging.cloud.detent.build; alias=staging.hub.detent.build ;;
  production) host=cloud.detent.build; alias=hub.detent.build ;;
  *) exit 2 ;;
esac
[[ "$commit" =~ ^[a-f0-9]{40}$ ]]
case "$environment:$version" in
  staging:develop-[a-f0-9]*) [[ "$version" =~ ^develop-[a-f0-9]{7,40}$ ]] ;;
  *)
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-op\.[a-f0-9]{12})?$ ]]
    if [[ "$version" == *-op.* ]]; then
      test "${version##*-op.}" = "${commit:0:12}"
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
ssh -i "$ssh_dir/id" \
  -o IdentitiesOnly=yes \
  -o HostKeyAlias="$alias" \
  -o UserKnownHostsFile="$ssh_dir/known_hosts" \
  -o StrictHostKeyChecking=yes \
  -o BatchMode=yes \
  -o ConnectTimeout=20 \
  "apprunner@$host" "deploy $commit" < "$scratch/release-hub/detent"
