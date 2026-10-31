#!/bin/bash
# Forced command for the GitHub Actions production deploy key. Production paths only.
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
staging=$(mktemp -d "${TMPDIR:-/tmp}/$NAME-upload.XXXXXX")
trap 'rm -rf "$staging"' EXIT
head -c 314572800 > "$staging/deploy.tar"
staged="$staging/detent"
tar -xOf "$staging/deploy.tar" detent > "$staged"
tar -xOf "$staging/deploy.tar" litestream-production.json > "$staging/production.yml"
size=$(stat -c %s "$staged")
[ "$size" -gt 1000000 ] || { echo "upload too small ($size bytes)" >&2; exit 1; }
chmod 0755 "$staging" "$staged"
reported=$(sudo -n -u $NAME "$staged" version --format json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("commit",""))')
[[ "$reported" == "$short"* ]] || { echo "binary reports commit '$reported', want $short" >&2; exit 1; }
sudo -n install -m 0644 -o root -g root "$staging/production.yml" /etc/litestream-detent/production.yml
sudo -n systemctl restart litestream-detent@production.service
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
healthy() {
  local version
  if [ "$unit" = "$NAME" ]; then
    version=$(sudo -n sh -c "umask 077; . /etc/$NAME/credentials.env; h=\$(mktemp); printf 'Authorization: Bearer %s' \"\$DETENT_HUB_ADMIN_TOKEN\" > \$h; curl -fsS --max-time 10 -H @\$h https://$DOMAIN/health; rc=\$?; rm -f \$h; exit \$rc" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("commit","") if d.get("status")=="ok" else "")' 2>/dev/null)
    [[ "$version" == "$short"* ]]
  else
    local pid code
    pid=$(systemctl show -p MainPID --value $unit)
    [ "${pid:-0}" != 0 ] && [ "$(sudo -n readlink /proc/$pid/exe)" = "$release" ] || return 1
    code=$(curl -s -o /dev/null --max-time 10 -w '%{http_code}' https://$DOMAIN/)
    [[ "$code" =~ ^(200|302|303)$ ]]
  fi
}
for _ in $(seq 1 30); do
  if healthy; then
    echo "$sha" | sudo -n tee "$STATE/DEPLOYED_SHA" >/dev/null
    ls -1t $OPT/releases | grep "^detent-ci-" | tail -n +6 | sed "s|^|$OPT/releases/|" | grep -vxF "$release" | xargs -r sudo -n rm -f || true
    echo "production deployed $short via $unit"
    exit 0
  fi
  sleep 2
done
wait_healthy() { for _ in $(seq 1 30); do healthy && return 0; sleep 2; done; return 1; }
echo "health check failed for $short; rolling back to ${previous:-nothing}" >&2
[ -n "$previous" ] || exit 1
release=$previous
sudo -n ln -sfn "$previous" "$OPT/detent"
restart_unit
short=$(basename "$previous" | sed -E 's/^detent-([0-9a-f]+).*/\1/')
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
