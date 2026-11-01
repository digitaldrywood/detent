#!/usr/bin/env bash
set -euo pipefail

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_file=$source_dir/restore-test.sh
destination=/usr/local/sbin/detent-restore-test
service=detent-restore-test.service
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
if cmp -s "$source_file" "$destination" && [ -x "$destination" ]; then
  echo "restore verifier is already installed"
  exit 0
fi
previous=false
if [ -e "$destination" ]; then
  cp -p "$destination" "$destination.previous"
  previous=true
fi
install -m 0755 "$source_file" "$destination.next"
mv -f "$destination.next" "$destination"
if ! systemctl restart "$service"; then
  if [ "$previous" = true ]; then
    cp -p "$destination.previous" "$destination.next"
    mv -f "$destination.next" "$destination"
    systemctl restart "$service" || echo "previous restore verifier also failed to start" >&2
  else
    mv -f "$destination" "$destination.failed"
  fi
  echo "restore verifier installation failed; previous file restored" >&2
  exit 1
fi
echo "installed restore verifier; previous file retained at $destination.previous"
