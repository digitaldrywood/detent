#!/usr/bin/env bash
set -euo pipefail
umask 077

check=false
case "${1:-}" in
  --check) check=true ;;
  "") ;;
  *) echo "usage: $0 [--check]" >&2; exit 2 ;;
esac
[ "$#" -le 1 ] || { echo "usage: $0 [--check]" >&2; exit 2; }

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_config=$source_dir/litestream-production.json
config=/etc/litestream-detent/production.yml
unit=litestream-detent@production.service
test -r "$source_config"
test -f "$config"
test ! -L "$config"
python3 -m json.tool "$source_config" >/dev/null

if "$check"; then
  if cmp -s "$source_config" "$config"; then
    echo "Litestream production configuration is current"
  else
    echo "Would back up $config, install $source_config and restart $unit"
  fi
  exit 0
fi

exec 9>/run/lock/detent-hub-deploy.lock
flock -w 600 9 || { echo "another production deployment holds the lock" >&2; exit 1; }
if cmp -s "$source_config" "$config" && systemctl is-active --quiet "$unit"; then
  echo "Litestream production configuration is current and active"
  exit 0
fi

backup=$(mktemp "$config.backup.XXXXXX")
cp -p "$config" "$backup"
staged=$(mktemp "$config.install.XXXXXX")
changed=false
finish() {
  local result=$?
  trap - EXIT
  if "$changed"; then
    echo "Activation failed; restoring $config from $backup" >&2
    if cp -p "$backup" "$staged" && mv -f "$staged" "$config"; then
      if systemctl restart "$unit" && systemctl is-active --quiet "$unit"; then
        echo "Previous Litestream configuration restored and service active" >&2
      else
        echo "Litestream recovery failed; previous configuration retained at $backup" >&2
      fi
    else
      echo "Configuration recovery failed; previous configuration retained at $backup" >&2
    fi
    result=1
  fi
  rm -f "$staged"
  exit "$result"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
install -m 0644 "$source_config" "$staged"
mv -f "$staged" "$config"
changed=true
systemctl restart "$unit"
systemctl is-active --quiet "$unit"
changed=false
echo "Litestream production configuration installed; previous configuration retained at $backup"
