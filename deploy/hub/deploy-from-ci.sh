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
if any(type(value) is not int or value < 1 for value in values) or any(type(value) is not str or len(value) != 64 or any(c not in "0123456789abcdef" for c in value) for value in fingerprints):
    sys.exit(1)
print(" ".join(map(str, values + fingerprints)))'
}

rollback_compatible() {
	local candidate_supported supported candidate_hub candidate_registry candidate_auth candidate_platform candidate_hub_fingerprint candidate_registry_fingerprint candidate_auth_fingerprint candidate_platform_fingerprint hub registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint
  [ -n "$previous" ] || return 1
	[ "${rollback_schema_known:-false}" = true ] || return 1
	[ "${applied_conflict:-false}" = false ] || return 1
	candidate_supported=$(binary_schema "$release") || return 1
  supported=$(binary_schema "$previous") || return 1
	read -r candidate_hub candidate_registry candidate_auth candidate_platform candidate_hub_fingerprint candidate_registry_fingerprint candidate_auth_fingerprint candidate_platform_fingerprint <<<"$candidate_supported"
	read -r hub registry auth platform hub_fingerprint registry_fingerprint auth_fingerprint platform_fingerprint <<<"$supported"
	[[ "$candidate_hub" =~ ^[0-9]+$ && "$candidate_registry" =~ ^[0-9]+$ && "$candidate_auth" =~ ^[0-9]+$ && "$candidate_platform" =~ ^[0-9]+$ && "$hub" =~ ^[0-9]+$ && "$registry" =~ ^[0-9]+$ && "$auth" =~ ^[0-9]+$ && "$platform" =~ ^[0-9]+$ ]] || return 1
	[[ "$candidate_hub" = "$hub" && "$candidate_registry" = "$registry" && "$candidate_auth" = "$auth" && "$candidate_platform" = "$platform" && "$candidate_hub_fingerprint" = "$hub_fingerprint" && "$candidate_registry_fingerprint" = "$registry_fingerprint" && "$candidate_auth_fingerprint" = "$auth_fingerprint" && "$candidate_platform_fingerprint" = "$platform_fingerprint" ]] || return 1
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
	release=$previous
	sudo -n ln -sfn "$previous" "$OPT/detent"
	restart_unit
	if wait_healthy; then
		echo "rolled back to $short" >&2
	else
		echo "previous binary is unhealthy; database left untouched; recovery required" >&2
	fi
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
  echo "health check failed; rollback schema is incompatible or unknown; candidate may have exited; recovery required" >&2
  exit 1
fi
echo "health check failed for $short; rolling back to ${previous:-nothing}" >&2
rollback_previous || exit 1
exit 1

}

if [[ "${BASH_SOURCE[0]}" = "$0" ]]; then main "$@"; fi
