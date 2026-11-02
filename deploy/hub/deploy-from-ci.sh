#!/bin/bash

release_commit() {
  basename "$1" | sed -nE 's/^detent-(ci-)?([0-9a-f]{7,40})(-.*)?$/\2/p'
}

binary_schema() {
  sudo -n -u "$NAME" "$1" version --format json 2>/dev/null | python3 -c '
import json, sys
schema = json.load(sys.stdin)
values = [schema.get(key) for key in ("hub_schema_version", "registry_schema_version", "auth_schema_version", "platform_schema_version")]
fingerprints = [schema.get(key) for key in ("hub_schema_fingerprint", "registry_schema_fingerprint", "auth_schema_fingerprint", "platform_schema_fingerprint")]
postgres_supported = schema.get("postgres_storage_supported", False)
if any(type(value) is not int or value < 1 for value in values) or any(type(value) is not str or len(value) != 64 or any(c not in "0123456789abcdef" for c in value) for value in fingerprints) or type(postgres_supported) is not bool:
    sys.exit(1)
print(" ".join(map(str, values + fingerprints + [str(postgres_supported).lower()])))'
}

postgres_cutover_active() {
	sudo -n test -f "$STATE/POSTGRES_CUTOVER"
}

binary_postgres_supported() {
	sudo -n -u "$NAME" "$1" version --format json 2>/dev/null | python3 -c 'import json,sys; v=json.load(sys.stdin); sys.exit(0 if v.get("postgres_storage_supported") is True else 1)'
}

rollback_compatible() {
	local candidate_supported supported candidate_hub candidate_registry candidate_auth candidate_platform candidate_hub_fingerprint candidate_registry_fingerprint candidate_auth_fingerprint candidate_platform_fingerprint candidate_postgres_supported postgres_supported hub registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint
  [ -n "$previous" ] || return 1
	[ "${rollback_schema_known:-false}" = true ] || return 1
	[ "${applied_conflict:-false}" = false ] || return 1
	candidate_supported=$(binary_schema "$release") || return 1
  supported=$(binary_schema "$previous") || return 1
	read -r candidate_hub candidate_registry candidate_auth candidate_platform candidate_hub_fingerprint candidate_registry_fingerprint candidate_auth_fingerprint candidate_platform_fingerprint candidate_postgres_supported <<<"$candidate_supported"
	read -r hub registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint postgres_supported <<<"$supported"
	[[ "$candidate_hub" =~ ^[0-9]+$ && "$candidate_registry" =~ ^[0-9]+$ && "$candidate_auth" =~ ^[0-9]+$ && "$candidate_platform" =~ ^[0-9]+$ && "$hub" =~ ^[0-9]+$ && "$registry" =~ ^[0-9]+$ && "$auth" =~ ^[0-9]+$ && "$platform" =~ ^[0-9]+$ ]] || return 1
	[[ "$candidate_hub" = "$hub" && "$candidate_registry" = "$registry" && "$candidate_auth" = "$auth" && "$candidate_platform" = "$platform" && "$candidate_hub_fingerprint" = "$hub_fingerprint" && "$candidate_registry_fingerprint" = "$registry_fingerprint" && "$candidate_auth_fingerprint" = "$auth_fingerprint" && "$candidate_platform_fingerprint" = "$platform_fingerprint" ]] || return 1
	if postgres_cutover_active; then [ "$candidate_postgres_supported" = true ] && [ "$postgres_supported" = true ] || return 1; fi
	if [ "$rollback_hub_applicable" = true ]; then [[ "$rollback_schema_version" =~ ^[1-9][0-9]*$ && "$rollback_schema_version" = "$candidate_hub" && "$candidate_hub" = "$hub" && "$rollback_schema_fingerprint" = "$candidate_hub_fingerprint" && "$candidate_hub_fingerprint" = "$hub_fingerprint" ]] || return 1; fi
	if [ "$rollback_registry_applicable" = true ]; then [[ "$rollback_registry_schema_version" =~ ^[1-9][0-9]*$ && "$rollback_registry_schema_version" = "$candidate_registry" && "$candidate_registry" = "$registry" && "$rollback_registry_schema_fingerprint" = "$candidate_registry_fingerprint" && "$candidate_registry_fingerprint" = "$registry_fingerprint" ]] || return 1; fi
	if [ "$rollback_auth_applicable" = true ]; then [[ "$rollback_auth_schema_version" =~ ^[1-9][0-9]*$ && "$rollback_auth_schema_version" = "$candidate_auth" && "$candidate_auth" = "$auth" && "$rollback_auth_schema_fingerprint" = "$candidate_auth_fingerprint" && "$candidate_auth_fingerprint" = "$auth_fingerprint" ]] || return 1; fi
	if [ "$rollback_platform_applicable" = true ]; then [[ "$rollback_platform_schema_version" =~ ^[1-9][0-9]*$ && "$rollback_platform_schema_version" = "$candidate_platform" && "$candidate_platform" = "$platform" && "$rollback_platform_schema_fingerprint" = "$candidate_platform_fingerprint" && "$candidate_platform_fingerprint" = "$platform_fingerprint" ]] || return 1; fi
}

read_health() {
	local pid
	pid=$(systemctl show -p MainPID --value "$unit")
	[ "${pid:-0}" != 0 ] && [ "$(sudo -n readlink "/proc/$pid/exe")" = "$release" ] || return 1
  if [ "$unit" = "$NAME" ]; then
    sudo -n sh -c "umask 077; . /etc/$NAME/credentials.env; h=\$(mktemp); printf 'Authorization: Bearer %s' \"\$DETENT_HUB_ADMIN_TOKEN\" > \$h; curl -sS --max-time 10 -H @\$h https://$DOMAIN/health; rc=\$?; rm -f \$h; exit \$rc" 2>/dev/null
  else
    curl -sS --max-time 10 "https://$DOMAIN/health"
  fi
}

parse_health() {
	python3 -c '
import json, sys
h = json.load(sys.stdin)
keys = ("schema_version", "registry_schema_version", "auth_schema_version", "platform_schema_version")
schemas = [h.get(key) for key in keys]
fingerprint_keys = ("schema_fingerprint", "registry_schema_fingerprint", "auth_schema_fingerprint", "platform_schema_fingerprint")
fingerprints = [h.get(key) or "-" for key in fingerprint_keys]
applicability_keys = ("hub_schema_applicable", "registry_schema_applicable", "auth_schema_applicable", "platform_schema_applicable")
applicability = [h.get(key) for key in applicability_keys]
facts_complete = h.get("schema_facts_complete")
commit = h.get("commit")
if not isinstance(commit, str) or not commit or any(type(value) is not int or value < 0 for value in schemas) or any(type(value) is not bool for value in applicability) or type(facts_complete) is not bool or any(type(value) is not str for value in fingerprints):
    sys.exit(1)
if any(applicable and (len(fingerprint) != 64 or any(c not in "0123456789abcdef" for c in fingerprint)) for applicable, fingerprint in zip(applicability, fingerprints)):
    sys.exit(1)
if type(h.get("migrations_running")) is not bool:
    sys.exit(1)
print("valid", h.get("status", "unavailable"), str(h["migrations_running"]).lower(), *schemas, *fingerprints, *(str(value).lower() for value in applicability), str(facts_complete).lower(), commit)'
}

capture_previous_schema() {
	local raw_health health_valid status running schema registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint hub_applicable registry_applicable auth_applicable platform_applicable facts_complete observed
	rollback_schema_known=false
	raw_health=$(read_health) || return 1
	health=$(printf '%s' "$raw_health" | parse_health) || return 1
	read -r health_valid status running schema registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint hub_applicable registry_applicable auth_applicable platform_applicable facts_complete observed <<<"$health"
	[[ "$health_valid" = valid && "$status" = ok && "$running" = false && "$facts_complete" = true && "$observed" = "$short"* && ( "$hub_applicable" != true || "$schema" =~ ^[1-9][0-9]*$ ) && ( "$registry_applicable" != true || "$registry" =~ ^[1-9][0-9]*$ ) && ( "$auth_applicable" != true || "$auth" =~ ^[1-9][0-9]*$ ) && ( "$platform_applicable" != true || "$platform" =~ ^[1-9][0-9]*$ ) ]] || return 1
	rollback_schema_known=true
	rollback_schema_version=$schema
	rollback_schema_fingerprint=$hub_fingerprint
	rollback_registry_schema_version=$registry
	rollback_registry_schema_fingerprint=$registry_fingerprint
	rollback_auth_schema_version=$auth
	rollback_auth_schema_fingerprint=$auth_fingerprint
	rollback_platform_schema_version=$platform
	rollback_platform_schema_fingerprint=$platform_fingerprint
	rollback_hub_applicable=$hub_applicable
	rollback_registry_applicable=$registry_applicable
	rollback_auth_applicable=$auth_applicable
	rollback_platform_applicable=$platform_applicable
}

wait_healthy() {
	local raw_health health health_valid status running schema registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint hub_applicable registry_applicable auth_applicable platform_applicable facts_complete observed sample_valid
	applied=0
	applied_registry=0
	applied_auth=0
	applied_platform=0
	applied_hub_fingerprint=
	applied_registry_fingerprint=
	applied_auth_fingerprint=
	applied_platform_fingerprint=
	applied_known=false
	applied_final=false
	applied_conflict=false
	: "${rollback_schema_known:=false}"
	: "${rollback_schema_version:=0}"
	: "${rollback_schema_fingerprint:=}"
	: "${rollback_registry_schema_version:=0}"
	: "${rollback_registry_schema_fingerprint:=}"
	: "${rollback_auth_schema_version:=0}"
	: "${rollback_auth_schema_fingerprint:=}"
	: "${rollback_platform_schema_version:=0}"
	: "${rollback_platform_schema_fingerprint:=}"
	: "${rollback_hub_applicable:=false}"
	: "${rollback_registry_applicable:=false}"
	: "${rollback_auth_applicable:=false}"
	: "${rollback_platform_applicable:=false}"
	applied_hub_applicable=false
	applied_registry_applicable=false
	applied_auth_applicable=false
	applied_platform_applicable=false
	while :; do
		local pid
		sample_valid=false
		applied_final=false
		pid=$(systemctl show -p MainPID --value "$unit")
		[ "${pid:-0}" != 0 ] && [ "$(sudo -n readlink "/proc/$pid/exe")" = "$release" ] || return 1
		raw_health=$(read_health 2>/dev/null) || raw_health=
		if [ -n "$raw_health" ]; then health=$(printf '%s' "$raw_health" | parse_health 2>/dev/null) || health="invalid unavailable false -1 -1 -1 -1 - - - - false false false false false unknown"; else health="missing unavailable false -1 -1 -1 -1 - - - - false false false false false unknown"; fi
		read -r health_valid status running schema registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint hub_applicable registry_applicable auth_applicable platform_applicable facts_complete observed <<<"$health"
		if [ "$health_valid" != missing ]; then rollback_schema_known=false; fi
	if [[ "$observed" = "$short"* && ( "$hub_applicable" != true || "$schema" =~ ^[1-9][0-9]*$ ) && ( "$registry_applicable" != true || "$registry" =~ ^[1-9][0-9]*$ ) && ( "$auth_applicable" != true || "$auth" =~ ^[1-9][0-9]*$ ) && ( "$platform_applicable" != true || "$platform" =~ ^[1-9][0-9]*$ ) ]]; then
	  sample_valid=true
	  if [ "$running" = true ] || [ "$facts_complete" != true ]; then applied_final=false; else applied_final=true; fi
	  if [ "$running" = false ] && [ "$facts_complete" = true ]; then
		    rollback_schema_known=true
		    rollback_schema_version=$schema
		    rollback_schema_fingerprint=$hub_fingerprint
		    rollback_registry_schema_version=$registry
		    rollback_registry_schema_fingerprint=$registry_fingerprint
		    rollback_auth_schema_version=$auth
		    rollback_auth_schema_fingerprint=$auth_fingerprint
		    rollback_platform_schema_version=$platform
		    rollback_platform_schema_fingerprint=$platform_fingerprint
		    rollback_hub_applicable=$hub_applicable
		    rollback_registry_applicable=$registry_applicable
		    rollback_auth_applicable=$auth_applicable
		    rollback_platform_applicable=$platform_applicable
		  fi
	  if [ "$applied_known" = true ] && { [ "$applied_hub_applicable" != "$hub_applicable" ] || [ "$applied_registry_applicable" != "$registry_applicable" ] || [ "$applied_auth_applicable" != "$auth_applicable" ] || [ "$applied_platform_applicable" != "$platform_applicable" ]; }; then
	    applied_known=false
	    applied_conflict=true
	  fi
	  if [ "$applied_known" = false ] && [ "$applied_conflict" = false ]; then
	    applied_hub_applicable=$hub_applicable
	    applied_registry_applicable=$registry_applicable
	    applied_auth_applicable=$auth_applicable
	    applied_platform_applicable=$platform_applicable
	    applied_known=true
	  fi
	  if [ "$hub_applicable" = true ] && (( schema > applied )); then applied=$schema; fi
	  if [ "$registry_applicable" = true ] && (( registry > applied_registry )); then applied_registry=$registry; fi
	  if [ "$auth_applicable" = true ] && (( auth > applied_auth )); then applied_auth=$auth; fi
	  if [ "$platform_applicable" = true ] && (( platform > applied_platform )); then applied_platform=$platform; fi
	  if [ "$hub_applicable" = true ]; then applied_hub_fingerprint=$hub_fingerprint; fi
	  if [ "$registry_applicable" = true ]; then applied_registry_fingerprint=$registry_fingerprint; fi
	  if [ "$auth_applicable" = true ]; then applied_auth_fingerprint=$auth_fingerprint; fi
	  if [ "$platform_applicable" = true ]; then applied_platform_fingerprint=$platform_fingerprint; fi
	fi
	    if [[ "$status" = ok && "$running" = false && "$facts_complete" = true && "$sample_valid" = true ]]; then return 0; fi
    if [[ "$running" = true && "$observed" = "$short"* ]]; then
      echo "startup migrations running; applied schema $applied" >&2
    fi
    sleep 2
	done
}

rollback_previous() {
	[ -n "$previous" ] || return 1
	short=$(release_commit "$previous")
	[ -n "$short" ] || { echo "previous release commit is unknown; candidate remains selected; recovery required" >&2; return 1; }
	if [ "${socket_bootstrap:-false}" = true ]; then
		if ! stop_unit; then
			echo "candidate service could not be stopped; recovery required" >&2
			return 1
		fi
		sudo -n systemctl stop "$socket_unit" || true
		sudo -n systemctl disable "$socket_unit" || true
		sudo -n rm -f "$socket_dropin" "$socket_path"
		sudo -n systemctl daemon-reload || true
		socket_bootstrap=false
	fi
	release=$previous
	sudo -n ln -sfn "$previous" "$OPT/detent"
	if ! restart_unit; then
		echo "previous service could not be restarted; recovery required" >&2
		return 1
	fi
	if wait_healthy; then
		echo "rolled back to $short" >&2
	else
		echo "previous binary is unhealthy; database left untouched; recovery required" >&2
	fi
}

restore_previous_before_candidate() {
	[ -n "$previous" ] || return 1
	short=$(release_commit "$previous")
	[ -n "$short" ] || return 1
	if postgres_cutover_active && ! rollback_compatible; then
		echo "previous release is not proven compatible with the PostgreSQL cutover; recovery required" >&2
		return 1
	fi
	if ! stop_unit; then
		echo "candidate service could not be stopped before restoration" >&2
		return 1
	fi
	if [ "${socket_bootstrap:-false}" = true ]; then
		sudo -n systemctl stop "$socket_unit" || true
		sudo -n systemctl disable "$socket_unit" || true
		sudo -n rm -f "$socket_dropin" "$socket_path"
		sudo -n systemctl daemon-reload || true
		socket_bootstrap=false
	fi
	release=$previous
	sudo -n ln -sfn "$previous" "$OPT/detent" || return 1
	sudo -n systemctl start "$unit" || return 1
	if wait_previous_healthy_before_candidate; then
		echo "restored previous service before candidate startup" >&2
		return 0
	fi
	echo "previous service did not recover; database left untouched; recovery required" >&2
	return 1
}

wait_previous_healthy_before_candidate() {
	local raw_health pid
	while :; do
		pid=$(systemctl show -p MainPID --value "$unit")
		[ "${pid:-0}" != 0 ] && [ "$(sudo -n readlink "/proc/$pid/exe")" = "$release" ] || return 1
		raw_health=$(read_health 2>/dev/null) || raw_health=
		if [ -n "$raw_health" ] && printf '%s' "$raw_health" | python3 -c 'import json,sys; h=json.load(sys.stdin); commit=h.get("commit"); sys.exit(0 if h.get("status") == "ok" and isinstance(commit,str) and commit.startswith(sys.argv[1]) else 1)' "$short"; then
			return 0
		fi
		sleep 2
	done
}

stop_unit() {
	local active_state pgrep_status
	sudo -n systemctl stop "$unit" || return 1
	for _ in $(seq 1 60); do
		if pgrep -u "$NAME" -f "$OPT/" >/dev/null; then
			sleep 1
			continue
		else
			pgrep_status=$?
		fi
		[ "$pgrep_status" = 1 ] || return 1
		break
	done
	if pgrep -u "$NAME" -f "$OPT/" >/dev/null; then return 1; else pgrep_status=$?; fi
	[ "$pgrep_status" = 1 ] || return 1
	active_state=$(systemctl show -p ActiveState --value "$unit") || return 1
	[ "$active_state" = inactive ] || return 1
	return 0
}

socket_address() {
	if [ "$unit" = "$CLOUD" ]; then
		case "$DOMAIN" in
			cloud.detent.build) printf '127.0.0.1:8017' ;;
			staging.cloud.detent.build) printf '127.0.0.1:8018' ;;
			*) return 1 ;;
		esac
		return
	fi
	printf '127.0.0.1:7777'
}

render_socket_unit() {
	cat <<EOF
[Unit]
Description=Detent $unit listener

[Socket]
ListenStream=$1
Accept=no
Backlog=65535

[Install]
WantedBy=sockets.target
EOF
}

render_socket_dropin() {
	local include_type=$1
	cat <<EOF
[Unit]
Requires=$socket_unit
After=$socket_unit

[Service]
EOF
	if [ "$include_type" = true ]; then printf 'Type=exec\n'; fi
	printf 'Sockets=%s\n' "$socket_unit"
}

install_socket_dropin_atomically() {
	local source=$1
	socket_dropin_stage="$socket_dropin.deploy.$$"
	if sudo -n test -e "$socket_dropin_stage"; then
		socket_dropin_stage=
		return 1
	fi
	if ! sudo -n install -m 0644 "$source" "$socket_dropin_stage"; then
		sudo -n rm -f "$socket_dropin_stage"
		socket_dropin_stage=
		return 1
	fi
	if ! sudo -n mv -f "$socket_dropin_stage" "$socket_dropin"; then
		sudo -n rm -f "$socket_dropin_stage"
		socket_dropin_stage=
		return 1
	fi
	socket_dropin_stage=
}

prepare_active_socket() {
	local address socket_file_status dropin_file_status
	address=$(socket_address) || { echo "no socket address is configured for $unit" >&2; return 1; }
	socket_file=$(mktemp "${TMPDIR:-/tmp}/$NAME-socket.XXXXXX")
	dropin_file=$(mktemp "${TMPDIR:-/tmp}/$NAME-dropin.XXXXXX")
	legacy_dropin_file=$(mktemp "${TMPDIR:-/tmp}/$NAME-legacy-dropin.XXXXXX")
	render_socket_unit "$address" > "$socket_file"
	render_socket_dropin true > "$dropin_file"
	render_socket_dropin false > "$legacy_dropin_file"
	if sudo -n test -L "$socket_path" || ! sudo -n test -f "$socket_path" || ! sudo -n cmp -s "$socket_file" "$socket_path"; then
		echo "active socket unit is not receiver-owned; refusing to replace its service configuration" >&2
		return 1
	fi
	if sudo -n test -L "$socket_dropin" || ! sudo -n test -f "$socket_dropin"; then
		echo "active socket service drop-in is not receiver-owned; refusing to replace it" >&2
		return 1
	fi
	if sudo -n cmp -s "$dropin_file" "$socket_dropin"; then
		sudo -n systemctl daemon-reload || { echo "active socket service configuration could not be reloaded" >&2; return 1; }
		return 0
	else
		dropin_file_status=$?
	fi
	if [ "$dropin_file_status" -ne 1 ]; then
		echo "active socket service drop-in could not be verified" >&2
		return 1
	fi
	if sudo -n cmp -s "$legacy_dropin_file" "$socket_dropin"; then
		if ! install_socket_dropin_atomically "$dropin_file"; then
			echo "active socket service drop-in could not be upgraded" >&2
			return 1
		fi
		if sudo -n systemctl daemon-reload; then return 0; fi
		if ! install_socket_dropin_atomically "$legacy_dropin_file" || ! sudo -n systemctl daemon-reload; then
			echo "active socket service drop-in reload failed; previous service remains running" >&2
			return 1
		fi
		echo "active socket service configuration reload failed; previous configuration restored" >&2
		return 1
	else
		socket_file_status=$?
	fi
	if [ "$socket_file_status" -ne 1 ]; then
		echo "active socket service drop-in could not be verified" >&2
	else
		echo "active socket service drop-in is not receiver-owned; refusing to replace it" >&2
	fi
	return 1
}

prepare_socket_activation() {
	socket_unit="$unit.socket"
	socket_path="/etc/systemd/system/$socket_unit"
	socket_dropin="/etc/systemd/system/$unit.service.d/socket.conf"
	socket_bootstrap=false
	if systemctl is-active --quiet "$socket_unit"; then
		prepare_active_socket
		return $?
	else
		local active_status=$?
		if [ "$active_status" -ne 3 ] && [ "$active_status" -ne 4 ]; then
			echo "socket activation state could not be verified" >&2
			return 1
		fi
	fi
	if sudo -n test -e "$socket_path" || sudo -n test -e "$socket_dropin"; then
		echo "inactive socket activation files already exist; refusing to replace them" >&2
		return 1
	fi
	local address dropin_directory
	address=$(socket_address) || { echo "no socket address is configured for $unit" >&2; return 1; }
	socket_file=$(mktemp "${TMPDIR:-/tmp}/$NAME-socket.XXXXXX")
	dropin_file=$(mktemp "${TMPDIR:-/tmp}/$NAME-dropin.XXXXXX")
	dropin_directory=${socket_dropin%/*}
	render_socket_unit "$address" > "$socket_file"
	render_socket_dropin true > "$dropin_file"
	if ! sudo -n install -d -m 0755 "$dropin_directory" || ! sudo -n install -m 0644 "$socket_file" "$socket_path" || ! sudo -n install -m 0644 "$dropin_file" "$socket_dropin" || ! sudo -n systemctl daemon-reload; then
		sudo -n rm -f "$socket_dropin" "$socket_path"
		sudo -n systemctl daemon-reload || true
		rm -f "$socket_file" "$dropin_file"
		socket_file= dropin_file=
		return 1
	fi
	rm -f "$socket_file" "$dropin_file"
	socket_file= dropin_file=
	socket_bootstrap=true
}

restart_unit() {
	candidate_start_state=not_started
	if [ "${socket_bootstrap:-false}" != true ]; then
		candidate_start_state=may_have_started
	fi
	if ! stop_unit; then
		candidate_start_state=may_have_started
		echo "service stop could not be verified" >&2
		return 1
	fi
	if [ "${socket_bootstrap:-false}" = true ]; then
		if ! sudo -n systemctl enable "$socket_unit"; then
			echo "socket activation could not be enabled" >&2
			return 1
		fi
		candidate_start_state=may_have_started
		if ! sudo -n systemctl start "$socket_unit"; then
			echo "socket activation could not be started" >&2
			return 1
		fi
	fi
	sudo -n systemctl start "$unit" || return 1
	candidate_start_state=started
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
if { [ "${verb:-}" != deploy ] && [ "${verb:-}" != cutover-postgres ]; } || [ -n "${extra:-}" ] || ! [[ "${sha:-}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "usage: deploy|cutover-postgres <40-hex commit sha> < upload" >&2
  exit 2
fi
short=${sha:0:9}
exec 9>/run/lock/$NAME-deploy.lock
flock -w 600 9 || { echo "another production deploy holds the lock" >&2; exit 1; }
staged=$(mktemp "${TMPDIR:-/tmp}/$NAME-upload.XXXXXX")
socket_file= dropin_file= legacy_dropin_file= socket_dropin_stage=
cutover_stage=
cutover_source_stage=
trap 'rm -f "$staged" "${socket_file:-}" "${dropin_file:-}" "${legacy_dropin_file:-}"; if [ -n "${socket_dropin_stage:-}" ]; then sudo -n rm -f "$socket_dropin_stage" || true; fi; if [ -n "${cutover_source_stage:-}" ]; then sudo -n rm -rf "$cutover_source_stage" || true; fi; if [ -n "${cutover_stage:-}" ]; then rm -rf "$cutover_stage"; fi' EXIT
if [ "$verb" = cutover-postgres ]; then
	max_payload=786432000
	head -c "$((max_payload + 1))" > "$staged"
	size=$(stat -c %s "$staged")
	[ "$size" -gt 1000000 ] && [ "$size" -le "$max_payload" ] || { echo "cutover bundle size is invalid" >&2; exit 1; }
	cutover_stage=$(mktemp -d "/tmp/$NAME-cutover.XXXXXX")
	python3 - "$staged" "$cutover_stage" <<'PY' || { echo "cutover bundle is invalid" >&2; exit 1; }
import pathlib
import shutil
import sys
import tarfile

bundle = pathlib.Path(sys.argv[1])
destination = pathlib.Path(sys.argv[2])
expected = {"detent", "cutover-postgres.sh", "source.bundle"}
with tarfile.open(bundle, "r:*") as archive:
    members = archive.getmembers()
    if len(members) != len(expected) or {member.name for member in members} != expected:
        raise SystemExit(1)
    for member in members:
        maximum = {"detent": 314572800, "cutover-postgres.sh": 1048576, "source.bundle": 419430400}[member.name]
        if not member.isfile() or member.mode & 0o7000 or member.size < 1 or member.size > maximum:
            raise SystemExit(1)
        source = archive.extractfile(member)
        if source is None:
            raise SystemExit(1)
        with source, (destination / member.name).open("xb") as output:
            shutil.copyfileobj(source, output, 1 << 20)
        if (destination / member.name).stat().st_size != member.size:
            raise SystemExit(1)
        mode = 0o755 if member.name == "detent" else 0o644
        (destination / member.name).chmod(mode)
destination.chmod(0o755)
PY
	cutover_source="$OPT/sources/$sha"
	cutover_source_stage=
	sudo -n install -d -o root -g "$NAME" -m 0750 "$OPT/sources"
	verify_cutover_source() {
		sudo -n test -d "$1" && ! sudo -n test -L "$1" || return 1
		[ "$(sudo -n -u "$NAME" env GIT_OPTIONAL_LOCKS=0 git -c safe.directory="$1" -C "$1" rev-parse --verify 'HEAD^{commit}')" = "$sha" ] || return 1
		[ -z "$(sudo -n -u "$NAME" env GIT_OPTIONAL_LOCKS=0 git -c safe.directory="$1" -C "$1" status --porcelain --untracked-files=all)" ] || return 1
	}
	if sudo -n test -e "$cutover_source" || sudo -n test -L "$cutover_source"; then
		verify_cutover_source "$cutover_source" || { echo "the existing frozen source checkout differs or is dirty" >&2; exit 1; }
	else
		cutover_source_stage="$OPT/sources/.cutover-$sha-$$"
		if sudo -n test -e "$cutover_source_stage" || sudo -n test -L "$cutover_source_stage"; then
			echo "the temporary source checkout path already exists" >&2
			exit 1
		fi
		sudo -n install -d -o "$NAME" -g "$NAME" -m 0700 "$cutover_source_stage"
		sudo -n -u "$NAME" git clone --quiet --no-checkout "$cutover_stage/source.bundle" "$cutover_source_stage" || { echo "the frozen source checkout could not be created" >&2; exit 1; }
		source_heads=$(sudo -n -u "$NAME" git -C "$cutover_source_stage" bundle list-heads "$cutover_stage/source.bundle") || { echo "the frozen source bundle references are unavailable" >&2; exit 1; }
		[ "$source_heads" = "$sha HEAD" ] || { echo "the frozen source bundle is not bound to the candidate commit" >&2; exit 1; }
		sudo -n -u "$NAME" git -C "$cutover_source_stage" bundle verify "$cutover_stage/source.bundle" >/dev/null || { echo "the frozen source bundle is invalid" >&2; exit 1; }
		printf '%s\n' "$sha" | sudo -n -u "$NAME" tee "$cutover_source_stage/.git/shallow" >/dev/null
		sudo -n -u "$NAME" git -C "$cutover_source_stage" checkout --quiet --detach "$sha" || { echo "the frozen source commit could not be checked out" >&2; exit 1; }
		verify_cutover_source "$cutover_source_stage" || { echo "the received source checkout is not exact and clean" >&2; exit 1; }
		sudo -n -u "$NAME" cmp -s "$cutover_stage/cutover-postgres.sh" "$cutover_source_stage/deploy/hub/cutover-postgres.sh" || { echo "the uploaded receiver does not match its frozen source" >&2; exit 1; }
		sudo -n chown -R "root:$NAME" "$cutover_source_stage"
		sudo -n chmod -R u=rwX,go=rX "$cutover_source_stage"
		sudo -n mv -T -- "$cutover_source_stage" "$cutover_source"
		cutover_source_stage=
	fi
	cutover_source=$(sudo -n readlink -f "$cutover_source") || { echo "the frozen release source cannot be resolved" >&2; exit 1; }
	[ "$cutover_source" = "$OPT/sources/$sha" ] || { echo "the frozen release source path is not canonical" >&2; exit 1; }
	verify_cutover_source "$cutover_source" || { echo "the frozen source checkout does not match the candidate" >&2; exit 1; }
	sudo -n cmp -s "$cutover_stage/cutover-postgres.sh" "$cutover_source/deploy/hub/cutover-postgres.sh" || { echo "the uploaded receiver does not match the frozen release source" >&2; exit 1; }
	cutover_metadata=$(sudo -n -u "$NAME" "$cutover_stage/detent" version --format json 2>/dev/null) || { echo "the cutover candidate version is unavailable" >&2; exit 1; }
	printf '%s' "$cutover_metadata" | python3 -c 'import json,sys; data=json.load(sys.stdin); sys.exit(0 if data.get("commit")==sys.argv[1] and data.get("postgres_storage_supported") is True and data.get("socket_activation_supported") is True else 1)' "$sha" || { echo "the cutover candidate does not match the exact PostgreSQL-capable release" >&2; exit 1; }
	for command in "cloud import-platform-postgres" "hub import-core-postgres"; do
		read -r group name <<<"$command"
		sudo -n -u "$NAME" "$cutover_stage/detent" "$group" "$name" --help >/dev/null || { echo "the cutover candidate lacks a required importer" >&2; exit 1; }
	done
	for path in /etc/detent-hub/cloud.postgres.yaml /etc/detent-hub/cloud.yaml /etc/detent-hub/cloud.env /etc/detent-hub/attachments.env; do
		if ! sudo -n test -f "$path" || sudo -n test -L "$path"; then
			echo "a provisioned cutover configuration is unavailable" >&2
			exit 1
		fi
	done
	timestamp=$(date -u +%Y%m%dT%H%M%SZ)
	candidate_release="$OPT/releases/detent-ci-$sha-$timestamp"
	receiver_release="$OPT/releases/detent-cutover-$sha-$timestamp.sh"
	if ! sudo -n test ! -e "$candidate_release" || ! sudo -n test ! -L "$candidate_release"; then
		echo "the immutable cutover candidate path already exists" >&2
		exit 1
	fi
	if ! sudo -n test ! -e "$receiver_release" || ! sudo -n test ! -L "$receiver_release"; then
		echo "the immutable cutover receiver path already exists" >&2
		exit 1
	fi
	sudo -n install -m 0755 -o root -g root "$cutover_stage/detent" "$candidate_release"
	sudo -n install -m 0555 -o root -g root "$cutover_stage/cutover-postgres.sh" "$receiver_release"
	sudo -n install -d -o root -g "$NAME" -m 0750 "$STATE/cutover"
	sudo -n env DETENT_DEPLOY_LOCK_HELD=true "$receiver_release" \
		--binary "$candidate_release" \
		--entry-config /etc/detent-hub/cloud.postgres.yaml \
		--sqlite-config /etc/detent-hub/cloud.yaml \
		--repository-root "$cutover_source" \
		--cloud-env /etc/detent-hub/cloud.env \
		--attachments-env /etc/detent-hub/attachments.env \
		--source-state "$STATE/cloud" \
		--tenant-root "$STATE/tenants" \
		--backup-root "$STATE/cutover/$sha" \
		--health-url https://cloud.detent.build/health
	exit $?
fi
head -c 314572800 > "$staged"
size=$(stat -c %s "$staged")
[ "$size" -gt 1000000 ] || { echo "upload too small ($size bytes)" >&2; exit 1; }
chmod 0755 "$staged"
reported=$(sudo -n -u $NAME "$staged" version --format json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("commit",""))')
[[ "$reported" == "$short"* ]] || { echo "binary reports commit '$reported', want $short" >&2; exit 1; }
socket_activation_supported=$(sudo -n -u "$NAME" "$staged" version --format json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("socket_activation_supported") is True)')
[ "$socket_activation_supported" = True ] || { echo "candidate binary does not declare socket activation support" >&2; exit 1; }
if postgres_cutover_active && ! binary_postgres_supported "$staged"; then
	echo "candidate binary does not declare PostgreSQL storage support after cutover" >&2
	exit 1
fi
release=$OPT/releases/detent-ci-$short-$(date -u +%Y%m%dT%H%M%SZ)
sudo -n install -m 0755 -o root -g root "$staged" "$release"
candidate_release=$release
candidate_short=$short
previous=$(readlink "$OPT/detent" || true)
if systemctl is-active --quiet $CLOUD; then unit=$CLOUD; else unit=$NAME; fi
rollback_schema_known=false
if [ -n "$previous" ]; then
  previous_short=$(release_commit "$previous" || true)
  if [ -n "$previous_short" ]; then
    release=$previous
    short=$previous_short
    if ! capture_previous_schema; then rollback_schema_known=false; fi
  fi
fi
release=$candidate_release
short=$candidate_short
if ! prepare_socket_activation; then
	if [ -n "$previous" ]; then sudo -n ln -sfn "$previous" "$OPT/detent"; fi
	echo "socket activation configuration could not be prepared; previous service remains selected" >&2
	exit 1
fi
if ! sudo -n ln -sfn "$release" "$OPT/detent"; then
	if [ "$socket_bootstrap" = true ]; then
		sudo -n rm -f "$socket_dropin" "$socket_path"
		sudo -n systemctl daemon-reload || true
		socket_bootstrap=false
	fi
	echo "candidate release could not be selected; previous service remains selected" >&2
	exit 1
fi
if ! restart_unit; then
	if [ "$candidate_start_state" = not_started ]; then
		echo "candidate could not start; restoring previous direct-binding service" >&2
		restore_previous_before_candidate || exit 1
		exit 1
	fi
	echo "candidate startup reported a failure; checking health before rollback" >&2
fi
if wait_healthy; then
  echo "$sha" | sudo -n tee "$STATE/DEPLOYED_SHA" >/dev/null
  ls -1t $OPT/releases | grep "^detent-ci-" | tail -n +6 | sed "s|^|$OPT/releases/|" | grep -vxF "$release" | xargs -r sudo -n rm -f || true
  echo "production deployed $short via $unit"
  exit 0
fi
if ! rollback_compatible; then
  echo "health check failed; rollback schema is incompatible or unknown; candidate may have exited; recovery required" >&2
  exit 1
fi
echo "health check failed for $short; rolling back to ${previous:-nothing}" >&2
rollback_previous || exit 1
exit 1

}

if [[ "${BASH_SOURCE[0]}" = "$0" ]]; then main "$@"; fi
