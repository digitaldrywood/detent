#!/usr/bin/env bash
set -uo pipefail

restore_target() {
  local target=$1 work=$2 bucket=$3 endpoint=$4 keep=${5:-false}
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
  rm -f "$out.log" "$out.check.log" "$out.lock" "$out-wal" "$out-shm" || return 1
  if [ "$keep" != true ] || [ "$result" != ok ]; then
    rm -f "$out" || return 1
  fi
  [ "$result" = ok ]
}

tenant_batch() {
  python3 - "$1" "$2" <<'PYTHON'
import pathlib
import re
import sqlite3
import sys

registry, cursor_path = map(pathlib.Path, sys.argv[1:])
try:
    cursor = cursor_path.read_text().strip()
except FileNotFoundError:
    cursor = ""
if cursor and not re.fullmatch(r"org_[a-zA-Z0-9_-]{1,60}", cursor):
    raise ValueError("invalid restore cursor")
with sqlite3.connect(registry.resolve().as_uri() + "?mode=ro&immutable=1", uri=True) as db:
    batch = db.execute("SELECT id FROM organizations WHERE state IN ('ready', 'disabled') AND id > ? ORDER BY id LIMIT 10", (cursor,)).fetchall()
    if len(batch) < 10:
        batch += db.execute("SELECT id FROM organizations WHERE state IN ('ready', 'disabled') AND id <= ? ORDER BY id LIMIT ?", (cursor, 10 - len(batch))).fetchall()
for (tenant,) in batch:
    if not re.fullmatch(r"org_[a-zA-Z0-9_-]{1,60}", tenant):
        raise ValueError("invalid restore tenant ID")
for (tenant,) in batch:
    print(tenant)
PYTHON
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
  local failed=0 tenant batch registry_ok=0
  restore_target cloud/registry.db "$work" "$bucket" "$endpoint" true || registry_ok=1
  restore_target cloud/auth.db "$work" "$bucket" "$endpoint" || failed=1
  [ "$registry_ok" = 0 ] || return 1
  batch=$(tenant_batch "$work/cloud_registry.db" "$work/tenant.cursor") || {
    rm -f "$work/cloud_registry.db"
    return 1
  }
  rm -f "$work/cloud_registry.db" || return 1
  while IFS= read -r tenant; do
    [ -n "$tenant" ] || continue
    restore_target "tenants/$tenant/hub.db" "$work" "$bucket" "$endpoint" || failed=1
    printf '%s\n' "$tenant" > "$work/tenant.cursor.next" || return 1
    mv -f "$work/tenant.cursor.next" "$work/tenant.cursor" || return 1
  done <<<"$batch"
  return "$failed"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
