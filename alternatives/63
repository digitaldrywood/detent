#!/usr/bin/env bash
set -euo pipefail

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_file=$source_dir/restore-test.sh
destination=/usr/local/sbin/detent-restore-test
service=detent-restore-test.service
verifier=/opt/detent-hub/detent
check=false
case "${1:-}" in
  --check) check=true ;;
  '') ;;
  *) echo "usage: $0 [--check]" >&2; exit 2 ;;
esac
[ "$#" -le 1 ] || { echo "usage: $0 [--check]" >&2; exit 2; }
bash -n "$source_file"
for command in python3 systemctl install cmp cp mv; do
  command -v "$command" >/dev/null
done
systemctl cat "$service" >/dev/null
if [ "$check" = true ]; then
  echo "would install $source_file at $destination and restart $service if changed"
  exit 0
fi
[ "$EUID" -eq 0 ] || { echo "run as root to install the restore verifier" >&2; exit 1; }
capabilities=$("$verifier" hub verify --help) || { echo "restore verifier binary is unavailable" >&2; exit 1; }
case "$capabilities" in
  *--integrity-only*) ;;
  *) echo "restore verifier binary does not support integrity-only checks" >&2; exit 1 ;;
esac
if cmp -s "$source_file" "$destination" && [ -x "$destination" ]; then
  result=$(systemctl show --property=Result --value "$service")
  [ -n "$result" ] || { echo "restore verifier service result is unavailable" >&2; exit 1; }
  if [ "$result" = success ]; then
    echo "restore verifier is already installed"
    exit 0
  fi
else
  if [ -e "$destination" ]; then
    cp -p "$destination" "$destination.previous"
  fi
  install -m 0755 "$source_file" "$destination.next"
  mv -f "$destination.next" "$destination"
fi
if ! systemctl restart "$service"; then
  echo "restore verification failed; installed verifier retained and previous file preserved" >&2
  exit 1
fi
echo "installed restore verifier; previous file retained at $destination.previous"
