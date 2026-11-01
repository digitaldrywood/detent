#!/bin/bash

release_commit() {
  basename "$1" | sed -nE 's/^detent-(ci-)?([0-9a-f]{7,40})(-.*)?$/\2/p'
}

binary_schema() {
  sudo -n -u "$NAME" "$1" version --format json 2>/dev/null | python3 -c '
import json, sys
v = json.load(sys.stdin)["hub_schema_version"]
if type(v) is not int or v <= 0:
    sys.exit(1)
print(v)'
}

rollback_compatible() {
  local supported candidate
  [ -n "$previous" ] || return 1
  supported=$(binary_schema "$previous") || return 1
  candidate=$(binary_schema "$release") || return 1
  [[ "$supported" =~ ^[0-9]+$ && "$candidate" =~ ^[0-9]+$ ]] || return 1
  (( supported >= candidate && supported >= applied ))
}

read_health() {
  if [ "$unit" = "$NAME" ]; then
    sudo -n sh -c "umask 077; . /etc/$NAME/credentials.env; h=\$(mktemp); printf 'Authorization: Bearer %s' \"\$DETENT_HUB_ADMIN_TOKEN\" > \$h; curl -sS --max-time 10 -H @\$h https://$DOMAIN/health; rc=\$?; rm -f \$h; exit \$rc" 2>/dev/null
  else
    local pid
    pid=$(systemctl show -p MainPID --value "$unit")
    [ "${pid:-0}" != 0 ] && [ "$(sudo -n readlink /proc/$pid/exe)" = "$release" ] || return 1
    curl -sS --max-time 10 "https://$DOMAIN/health"
  fi
}

wait_healthy() {
  local failures=0 health status running schema observed
  applied=${applied:-0}
  while (( failures < 30 )); do
    health=$(read_health | python3 -c '
import json, sys
h = json.load(sys.stdin)
s = h.get("schema_version", 0)
if type(s) is not int or s < 0:
    sys.exit(1)
print(h.get("status", "unavailable"), str(h.get("migrations_running") is True).lower(), s, h.get("commit") or "unknown")' 2>/dev/null) || health="unavailable false 0 unknown"
    read -r status running schema observed <<<"$health"
    (( schema > applied )) && applied=$schema
    if [[ "$status" = ok && "$observed" = "$short"* ]]; then return 0; fi
    if [[ "$running" = true && "$observed" = "$short"* ]]; then
      failures=0
      echo "startup migrations running; applied schema $applied" >&2
    else
      failures=$((failures + 1))
    fi
    sleep 2
  done
  return 1
}

main() {
set -euo pipefail
umask 022
NAME=detent-hub
CLOUD=detent-cloud
DOMAIN=cloud.detent.build
OPT=/opt/$NAME
STATE=/var/lib/$NAME
read -r verb sha extra <<<"${SSH_ORIGINAL_COMMAND:-}"
if [ "${verb:-}" != deploy ] || [ -n "${extra:-}" ] || ! [[ "${sha:-}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: deploy <40-hex commit sha> < binary" >&2
  exit 2
fi
short=${sha:0:9}
exec 9>/run/lock/$NAME-deploy.lock
flock -w 600 9 || { echo "another production deploy holds the lock" >&2; exit 1; }
staged=$(mktemp "${TMPDIR:-/tmp}/$NAME-upload.XXXXXX")
trap 'rm -f "$staged"' EXIT
head -c 314572800 > "$staged"
size=$(stat -c %s "$staged")
[ "$size" -gt 1000000 ] || { echo "upload too small ($size bytes)" >&2; exit 1; }
chmod 0755 "$staged"
reported=$(sudo -n -u $NAME "$staged" version --format json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("commit",""))')
[[ "$reported" == "$short"* ]] || { echo "binary reports commit '$reported', want $short" >&2; exit 1; }
release=$OPT/releases/detent-ci-$short-$(date -u +%Y%m%dT%H%M%SZ)
sudo -n install -m 0755 -o root -g root "$staged" "$release"
previous=$(readlink "$OPT/detent" || true)
if systemctl is-active --quiet $CLOUD; then unit=$CLOUD; else unit=$NAME; fi
# Shared-entry tenants and stores replicate live through litestream-detent; never stop them to snapshot.
snapshot=""
if [ "$unit" = "$NAME" ]; then
  snapshot=$(sudo -n /opt/detent-hub/backup.sh $NAME | sed -n 's/.* \(hub-[0-9TZ]*\.tar\.gz\) uploaded.*/\1/p')
  [ -n "$snapshot" ] || { echo "pre-deploy snapshot failed" >&2; exit 1; }
fi
sudo -n ln -sfn "$release" "$OPT/detent"
restart_unit() {
  sudo -n systemctl stop $unit || true
  for _ in $(seq 1 60); do pgrep -u $NAME -f "$OPT/" >/dev/null || break; sleep 1; done
  sudo -n systemctl start $unit || true
}
restart_unit
if wait_healthy; then
  echo "$sha" | sudo -n tee "$STATE/DEPLOYED_SHA" >/dev/null
  ls -1t $OPT/releases | grep "^detent-ci-" | tail -n +6 | sed "s|^|$OPT/releases/|" | grep -vxF "$release" | xargs -r sudo -n rm -f || true
  echo "production deployed $short via $unit"
  exit 0
fi
if ! rollback_compatible; then
  echo "health check failed; rollback schema is incompatible or unknown; keeping $release running" >&2
  exit 1
fi
echo "health check failed for $short; rolling back to ${previous:-nothing}" >&2
[ -n "$previous" ] || exit 1
short=$(release_commit "$previous")
[ -n "$short" ] || { echo "rollback release commit is unknown; keeping $release running" >&2; exit 1; }
release=$previous
sudo -n ln -sfn "$previous" "$OPT/detent"
restart_unit
if wait_healthy; then echo "rolled back to $short" >&2; exit 1; fi
if [ "$unit" = "$NAME" ]; then
  echo "previous binary is unhealthy too; restoring pre-deploy snapshot $snapshot" >&2
  sudo -n systemctl stop $unit
  work=$(sudo -n mktemp -d /var/lib/$NAME/restore.XXXXXX)
  sudo -n tar -C "$work" -xzf "$STATE/backups/$snapshot"
  sudo -n install -m 0600 -o $NAME -g $NAME "$work/${snapshot%.tar.gz}/hub.db" "$STATE/hub.db"
  sudo -n rm -rf "$work" "$STATE/hub.db-wal" "$STATE/hub.db-shm"
  sudo -n systemctl start $unit || true
  wait_healthy && echo "restored snapshot and rolled back to $short" >&2
fi
exit 1

}

if [[ "${BASH_SOURCE[0]}" = "$0" ]]; then main "$@"; fi
