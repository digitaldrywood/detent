#!/usr/bin/env bash
set -euo pipefail

source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
destination="${DESTDIR:-}/etc/systemd/system"

install -d "$destination/detent-cloud.service.d" "$destination/detent-hub.service.d"
install -m 0644 "$source_root/docs/examples/cloud/detent-cloud.socket" "$destination/detent-cloud.socket"
install -m 0644 "$source_root/docs/examples/cloud/detent-cloud.service.d/socket.conf" "$destination/detent-cloud.service.d/socket.conf"
install -m 0644 "$source_root/docs/examples/hub/detent-hub.socket" "$destination/detent-hub.socket"
install -m 0644 "$source_root/docs/examples/hub/detent-hub.service.d/socket.conf" "$destination/detent-hub.service.d/socket.conf"

if [[ -z "${DESTDIR:-}" ]]; then
	systemctl daemon-reload
fi
