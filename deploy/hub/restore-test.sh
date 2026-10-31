#!/usr/bin/env bash
set -uo pipefail

restore_target() {
  local target=$1 work=$2 bucket=$3 endpoint=$4
  local out started tables bytes result check
  out="$work/${target//\//_}"
  started=$(date +%s)
  result=corrupt
  tables=0
  rm -f "$out" "$out.log" "$out.check.log" "$out.lock" "$out-wal" "$out-shm" || return 1
  if /usr/local/bin/litestream-detent restore -o "$out" "$bucket/$target?$endpoint" >"$out.log" 2>&1; then
    if check=$(/opt/detent-hub/detent hub verify --integrity-only --database "$out" 2>"$out.check.log"); then
      if tables=$(python3 -c 'import json,sys; n=json.load(sys.stdin)["tables"]; assert type(n) is int and n > 0; print(n)' <<<"$check"); then
        result=ok
      else
        tables=0
      fi
    fi
  fi
  bytes=$(stat -c %s "$out" 2>/dev/null) || bytes=0
  echo "restore_test target=$target result=$result bytes=$bytes tables=$tables seconds=$(( $(date +%s) - started ))"
  rm -f "$out" "$out.log" "$out.check.log" "$out.lock" "$out-wal" "$out-shm" || return 1
  [ "$result" = ok ]
}

main() {
  umask 077
  set -a
  . /etc/litestream-detent/environment || return 1
  set +a
  local work=/var/tmp/detent-restore-test
  mkdir -p "$work" || return 1
  local endpoint="endpoint=https://nyc3.digitaloceanspaces.com&region=nyc3"
  local bucket=s3://drywood-detent-litestream/production
  local failed=0 target dir
  for target in cloud/registry.db cloud/auth.db; do
    restore_target "$target" "$work" "$bucket" "$endpoint" || failed=1
  done
  for dir in /var/lib/detent-hub/tenants/*/; do
    [ -d "$dir" ] || continue
    target="tenants/$(basename "$dir")/hub.db"
    restore_target "$target" "$work" "$bucket" "$endpoint" || failed=1
  done
  return "$failed"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
