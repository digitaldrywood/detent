#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

UNIT=detent-cloud
SOCKET=detent-cloud.socket
STATE=/var/lib/detent-hub
LOCK=/run/lock/detent-hub-deploy.lock
SERVICE_USER=detent-hub
SERVICE_GROUP=detent-hub
SETPRIV=/usr/bin/setpriv

usage() {
	cat >&2 <<'EOF'
usage: cutover-postgres.sh --binary PATH --entry-config PATH --sqlite-config PATH --repository-root PATH --cloud-env PATH --attachments-env PATH --source-state PATH --tenant-root PATH --backup-root PATH --health-url URL
EOF
}

fail() {
	echo "PostgreSQL cutover refused: $1" >&2
	exit 1
}

binary='' entry_config='' sqlite_config='' repository_root='' cloud_env='' attachments_env='' source_state='' tenant_root='' backup_root='' health_url=''
while [ "$#" -gt 0 ]; do
	case "$1" in
		--binary) binary=${2:-}; shift 2 ;;
		--entry-config) entry_config=${2:-}; shift 2 ;;
		--sqlite-config) sqlite_config=${2:-}; shift 2 ;;
		--repository-root) repository_root=${2:-}; shift 2 ;;
		--cloud-env) cloud_env=${2:-}; shift 2 ;;
		--attachments-env) attachments_env=${2:-}; shift 2 ;;
		--source-state) source_state=${2:-}; shift 2 ;;
		--tenant-root) tenant_root=${2:-}; shift 2 ;;
		--backup-root) backup_root=${2:-}; shift 2 ;;
		--health-url) health_url=${2:-}; shift 2 ;;
		*) usage; exit 2 ;;
	esac
done

for value in "$binary" "$entry_config" "$sqlite_config" "$repository_root" "$cloud_env" "$attachments_env" "$source_state" "$tenant_root" "$backup_root" "$health_url"; do
	[ -n "$value" ] || { usage; exit 2; }
done

[ -f "$binary" ] && [ ! -L "$binary" ] && [ -x "$binary" ] || fail "the selected release binary is unavailable"
[ -f "$entry_config" ] && [ ! -L "$entry_config" ] || fail "the PostgreSQL configuration is not a regular file"
[ -f "$sqlite_config" ] && [ ! -L "$sqlite_config" ] || fail "the active SQLite configuration is not a regular file"
[ -f "$cloud_env" ] && [ ! -L "$cloud_env" ] || fail "the provisioned Cloud environment file is unavailable"
[ -f "$attachments_env" ] && [ ! -L "$attachments_env" ] || fail "the provisioned archive environment file is unavailable"
[ -d "$repository_root" ] && [ ! -L "$repository_root" ] || fail "the exact source tree is unavailable"
[ -d "$source_state" ] && [ ! -L "$source_state" ] || fail "the shared source state directory is unavailable"
[ -d "$tenant_root" ] && [ ! -L "$tenant_root" ] || fail "the tenant root is unavailable"
[ "${backup_root#/}" != "$backup_root" ] || fail "the sealed-copy destination must be absolute"
[ "${backup_root%/}" = "$backup_root" ] || fail "the sealed-copy destination must be normalized"
[ -d "${backup_root%/*}" ] && [ ! -L "${backup_root%/*}" ] || fail "the sealed-copy parent directory is unavailable"
[ ! -e "$backup_root" ] || fail "the sealed-copy destination already exists"
[ ! -e "$STATE/POSTGRES_CUTOVER" ] || fail "the PostgreSQL cutover has already been attempted"
python3 - "$backup_root" "$source_state" "$tenant_root" "$repository_root" <<'PY' || fail "the sealed-copy destination overlaps source data"
import os
import sys
backup, *sources = (os.path.realpath(path) for path in sys.argv[1:])
for source in sources:
    common = os.path.commonpath((backup, source))
    if common == backup or common == source:
        raise SystemExit(1)
PY

if [ "${DETENT_DEPLOY_LOCK_HELD:-}" = true ]; then
	:
else
	exec 9>"$LOCK"
	flock -w 600 9 || fail "another production deployment owns the shared lock"
fi
[ ! -e "$backup_root" ] || fail "the sealed-copy destination already exists"
[ ! -e "$STATE/POSTGRES_CUTOVER" ] || fail "the PostgreSQL cutover has already been attempted"
if ! sudo -n test -d "$STATE" || sudo -n test -L "$STATE"; then fail "the Cloud state directory is unavailable"; fi

run_service() (
	[ -x "$SETPRIV" ] || fail "the service identity launcher is unavailable"
	local service_uid service_gid service_home
	service_uid=$(id -u "$SERVICE_USER") || fail "the service account is unavailable"
	service_gid=$(id -g "$SERVICE_USER") || fail "the service group is unavailable"
	service_home=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
	[ -n "$service_home" ] || fail "the service account home is unavailable"
	/bin/bash -c '
		set -Eeuo pipefail
		set -a
		. "$1"
		. "$2"
		shift 2
		service_uid=$1
		service_gid=$2
		service_home=$3
		service_user=$4
		shift 4
		export HOME="$service_home" USER="$service_user" LOGNAME="$service_user"
		exec /usr/bin/setpriv --reuid="$service_uid" --regid="$service_gid" --init-groups -- "$@"
	' cutover-env "$cloud_env" "$attachments_env" "$service_uid" "$service_gid" "$service_home" "$SERVICE_USER" "$@"
)

check_health() {
	run_service python3 - "$health_url" "${1:-}" <<'PY'
import json
import sys
import urllib.request
with urllib.request.urlopen(sys.argv[1], timeout=10) as response:
    health = json.load(response)
expected = sys.argv[2]
if expected:
    if health.get("status") != "ok" or health.get("schema_facts_complete") is not True or health.get("migrations_running") is not False or health.get("commit") != expected:
        raise SystemExit(1)
elif not isinstance(health.get("commit"), str) or health.get("status") not in ("ok", "needs_attention"):
    raise SystemExit(1)
PY
}

version_json=$(run_service "$binary" version --format json) || fail "the selected binary cannot report its version"
read -r commit postgres_supported socket_supported <<<"$(printf '%s' "$version_json" | python3 -c 'import json,sys; v=json.load(sys.stdin); c=v.get("commit"); p=v.get("postgres_storage_supported"); s=v.get("socket_activation_supported"); sys.exit(1) if not isinstance(c,str) or len(c)!=40 or any(x not in "0123456789abcdef" for x in c) or type(p) is not bool or type(s) is not bool else print(c,str(p).lower(),str(s).lower())')" || fail "the selected binary lacks required deployment capability metadata"
[ "$postgres_supported" = true ] || fail "the selected release does not support PostgreSQL storage"
[ "$socket_supported" = true ] || fail "the selected release does not support the active socket receiver"
source_commit=$(GIT_OPTIONAL_LOCKS=0 git -c safe.directory="$repository_root" -C "$repository_root" rev-parse --verify 'HEAD^{commit}') || fail "the selected source checkout is unavailable"
[ "$source_commit" = "$commit" ] || fail "the selected source checkout does not match the binary"
source_changes=$(GIT_OPTIONAL_LOCKS=0 git -c safe.directory="$repository_root" -C "$repository_root" status --porcelain --untracked-files=all) || fail "the selected source checkout cannot be verified"
[ -z "$source_changes" ] || fail "the selected source checkout is not clean"
cmp -s "$0" "$repository_root/deploy/hub/cutover-postgres.sh" || fail "the installed receiver differs from the exact source commit"
run_service "$binary" hub import-core-postgres --help >/dev/null || fail "the selected binary lacks the complete tenant importer"

current_link=/opt/detent-hub/detent
candidate_release=$(readlink -f "$binary") || fail "the selected release target cannot be resolved"
[ "$candidate_release" = "$binary" ] || fail "the selected binary path must be the immutable release file"
previous_release=$(readlink "$current_link") || fail "the active release link cannot be read"
[ -n "$previous_release" ] || fail "the active release link is empty"
previous_release=$(readlink -f "$previous_release") || fail "the active release target cannot be resolved"
[ -x "$previous_release" ] && [ ! -L "$previous_release" ] || fail "the active release target is not an immutable executable"

active_state=$(systemctl show -p ActiveState --value "$UNIT") || fail "the Cloud service state is unavailable"
socket_state=$(systemctl show -p ActiveState --value "$SOCKET") || fail "the Cloud socket state is unavailable"
[ "$active_state" = active ] && [ "$socket_state" = active ] || fail "the active Cloud service and socket are not both verified active"
kill_mode=$(systemctl show -p KillMode --value "$UNIT") || fail "the Cloud service kill mode is unavailable"
[ "$kill_mode" = control-group ] || fail "the Cloud service does not own its tenant children as a control group"
environment_files=$(systemctl show -p EnvironmentFiles --value "$UNIT") || fail "the Cloud environment file binding is unavailable"
python3 - "$environment_files" "$cloud_env" "$attachments_env" <<'PY' || fail "the provisioned environment files are not bound to the Cloud service"
import re
import sys
paths = re.findall(r"(\S+)\s+\(ignore_errors=[^)]+\)", sys.argv[1])
if sys.argv[2] not in paths or sys.argv[3] not in paths:
    raise SystemExit(1)
PY
check_health || fail "the current production Cloud health cannot be verified"

sudo -n install -d -o root -g "$SERVICE_GROUP" -m 0750 "$backup_root"
socket_stopped=false
service_stopped=false
children_clear=false
config_switched=false
release_switched=false
marker_written=false
postgres_start_attempted=false
complete=false
restored_sqlite=false

restore_sqlite_service() {
	if [ "$config_switched" = true ]; then
		sudo -n install -o root -g "$SERVICE_GROUP" -m 0640 "$backup_root/cloud.sqlite.yaml" "$sqlite_config.restore.$$" || return 1
		sudo -n mv -f "$sqlite_config.restore.$$" "$sqlite_config" || return 1
		config_switched=false
	fi
	if [ "$release_switched" = true ]; then
		sudo -n ln -sfn "$previous_release" "$current_link.restore.$$" || return 1
		sudo -n mv -f "$current_link.restore.$$" "$current_link" || return 1
		release_switched=false
	fi
	if ! sudo -n systemctl start "$SOCKET" || ! sudo -n systemctl start "$UNIT" || [ "$(systemctl show -p ActiveState --value "$UNIT")" != active ] || [ "$(systemctl show -p ActiveState --value "$SOCKET")" != active ]; then
		fence_for_reconciliation || true
		return 1
	fi
}

fence_for_reconciliation() {
	sudo -n systemctl stop "$SOCKET" >/dev/null 2>&1 || return 1
	sudo -n systemctl stop "$UNIT" >/dev/null 2>&1 || return 1
	[ "$(systemctl show -p ActiveState --value "$SOCKET")" = inactive ] || return 1
	[ "$(systemctl show -p ActiveState --value "$UNIT")" = inactive ] || return 1
	[ "$(systemctl show -p MainPID --value "$UNIT")" = 0 ] || return 1
}

read_cgroup_processes() {
	sudo -n python3 - "$1" <<'PY'
import pathlib
import sys
root = pathlib.Path(sys.argv[1])
direct = root / "cgroup.procs"
if not direct.is_file():
    raise SystemExit(1)
files = [direct, *(path for path in root.rglob("cgroup.procs") if path != direct)]
for path in files:
    for pid in path.read_text().split():
        if not pid.isdecimal():
            raise SystemExit(1)
        print(pid)
PY
}

on_exit() {
	status=$?
	trap - EXIT
	if [ "$complete" = true ]; then exit "$status"; fi
	if [ "$postgres_start_attempted" = true ]; then
		if fence_for_reconciliation; then
			echo "PostgreSQL startup was attempted; PostgreSQL configuration and sealed copies are retained, and the Cloud units remain fenced for reconciliation" >&2
		else
			echo "PostgreSQL startup was attempted, but the Cloud fence could not be verified; reconciliation is required" >&2
		fi
	elif [ "$service_stopped" = true ] && [ "$children_clear" = true ]; then
		if restore_sqlite_service; then
			restored_sqlite=true
		else
			echo "SQLite service restoration failed; the Cloud units remain fenced for reconciliation" >&2
		fi
	elif [ "$service_stopped" = true ]; then
		echo "the Cloud service stopped, but tenant child exit could not be verified; both Cloud units remain fenced" >&2
	elif [ "$socket_stopped" = true ]; then
		echo "the Cloud socket was stopped before service state was verified; both Cloud units remain fenced" >&2
	fi
	if [ "$marker_written" = true ] && [ "$postgres_start_attempted" != true ] && [ "$restored_sqlite" = true ]; then sudo -n rm -f "$STATE/POSTGRES_CUTOVER"; fi
	sudo -n rm -f "$current_link.next.$$" "$current_link.restore.$$" "$sqlite_config.next.$$" "$sqlite_config.restore.$$" || true
	exit "$status"
}
trap on_exit EXIT

pre_stop_pid=$(systemctl show -p MainPID --value "$UNIT") || fail "the active Cloud process ID is unavailable"
[ "$pre_stop_pid" != 0 ] || fail "the active Cloud service has no main process"
pre_stop_exe=$(sudo -n readlink "/proc/$pre_stop_pid/exe") || fail "the active Cloud executable is unavailable"
[ "$pre_stop_exe" = "$previous_release" ] || fail "the active Cloud process does not match the selected prior release"
pre_stop_group=$(systemctl show -p ControlGroup --value "$UNIT") || fail "the active Cloud control group is unavailable"
[ "$pre_stop_group" = "/system.slice/$UNIT.service" ] || fail "the active Cloud control group is unexpected"
pre_stop_group_path="/sys/fs/cgroup$pre_stop_group"
sudo -n test -d "$pre_stop_group_path" || fail "the active Cloud control group is unavailable"
pre_stop_processes=$(read_cgroup_processes "$pre_stop_group_path") || fail "the active Cloud child process list is unavailable"
pre_stop_processes=${pre_stop_processes//$'\n'/ }
case " $pre_stop_processes " in
	*" $pre_stop_pid "*) ;;
	*) fail "the active Cloud main process is outside its control group" ;;
esac
pre_stop_process_identity=$(sudo -n python3 - "$pre_stop_processes" <<'PY' || fail "the active Cloud process identities are unavailable"
import pathlib
import sys
for value in sys.argv[1].split():
    pid = int(value)
    raw = pathlib.Path(f"/proc/{pid}/stat").read_text()
    fields = raw[raw.rfind(")") + 2:].split()
    print(pid, fields[19])
PY
)

sudo -n systemctl stop "$SOCKET" || fail "the Cloud socket could not be stopped"
socket_stopped=true
sudo -n systemctl stop "$UNIT" || fail "the Cloud service could not be stopped"
active_state=$(systemctl show -p ActiveState --value "$UNIT") || fail "the stopped Cloud service state is unavailable"
socket_state=$(systemctl show -p ActiveState --value "$SOCKET") || fail "the stopped Cloud socket state is unavailable"
main_pid=$(systemctl show -p MainPID --value "$UNIT") || fail "the stopped Cloud main process state is unavailable"
control_group=$(systemctl show -p ControlGroup --value "$UNIT") || fail "the stopped Cloud control group is unavailable"
[ "$active_state" = inactive ] && [ "$socket_state" = inactive ] && [ "$main_pid" = 0 ] || fail "the Cloud socket or service remains active"
service_stopped=true
if [ -n "$control_group" ] && [ "$control_group" != "$pre_stop_group" ]; then fail "the Cloud service control group is unexpected"; fi
group_path="/sys/fs/cgroup$pre_stop_group"
if sudo -n test -d "$group_path"; then
	processes=$(read_cgroup_processes "$group_path") || fail "the Cloud control group process list is unavailable"
else
	parent_path="${group_path%/*}"
	sudo -n test -d "$parent_path" || fail "the Cloud control group state is unavailable"
	processes=
fi
[ -z "$processes" ] || fail "Cloud tenant children remain in the service control group"
for pid in $pre_stop_processes; do
	case " $processes " in *" $pid "*) fail "a previously observed Cloud process remains in the service control group" ;; esac
done
sudo -n python3 - "$pre_stop_process_identity" <<'PY' || fail "a previously observed Cloud process remains alive"
import pathlib
import sys
for value in sys.argv[1].splitlines():
    pid, start = value.split()
    try:
        raw = pathlib.Path(f"/proc/{pid}/stat").read_text()
    except FileNotFoundError:
        continue
    fields = raw[raw.rfind(")") + 2:].split()
    if fields[19] == start:
        raise SystemExit(1)
PY
children_clear=true

sudo -n install -d -o root -g "$SERVICE_GROUP" -m 0750 "$backup_root/platform" "$backup_root/tenants"
sudo -n install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0440 "$sqlite_config" "$backup_root/cloud.sqlite.yaml"
copy_sealed() {
	local source=$1 destination=$2
	if ! sudo -n test -f "$source" || sudo -n test -L "$source"; then fail "a registered SQLite source is not a regular file"; fi
	sudo -n python3 - "$source" "$destination" "$SERVICE_USER" "$SERVICE_GROUP" <<'PY'
import grp
import hashlib
import os
import pathlib
import pwd
import sqlite3
import sys
source, destination, user, group = sys.argv[1:]
source_path = pathlib.Path(source)
destination_path = pathlib.Path(destination)
source_info = os.lstat(source_path)
if not os.path.isfile(source_path) or os.path.islink(source_path) or os.path.lexists(destination_path):
    raise SystemExit(1)
def source_identity(path):
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return None
    if not os.path.isfile(path) or os.path.islink(path):
        raise SystemExit(1)
    identity = lambda value: (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns, value.st_ctime_ns)
    digest = hashlib.sha256()
    with open(path, "rb") as sidecar:
        opened = os.fstat(sidecar.fileno())
        if identity(info) != identity(opened):
            raise SystemExit(1)
        for chunk in iter(lambda: sidecar.read(1024 * 1024), b""):
            digest.update(chunk)
        after_read = os.fstat(sidecar.fileno())
    if identity(opened) != identity(after_read):
        raise SystemExit(1)
    return info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, digest.hexdigest()
source_sidecars = {suffix: source_identity(str(source_path) + suffix) for suffix in ("-wal", "-journal")}
reader = sqlite3.connect(source_path.resolve().as_uri() + "?mode=ro", uri=True)
writer = sqlite3.connect(destination_path)
try:
    reader.backup(writer)
    writer.commit()
finally:
    writer.close()
    reader.close()
after = os.lstat(source_path)
identity = lambda info: (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
destination_sidecars = (str(destination_path) + "-wal", str(destination_path) + "-journal")
current_sidecars = {suffix: source_identity(str(source_path) + suffix) for suffix in ("-wal", "-journal")}
previous_wal = source_sidecars["-wal"]
current_wal = current_sidecars["-wal"]
new_empty_wal = previous_wal is None and current_wal is not None and current_wal[2] == 0
source_sidecars_unchanged = source_sidecars["-journal"] == current_sidecars["-journal"] and (new_empty_wal or previous_wal == current_wal)
if identity(source_info) != identity(after) or not source_sidecars_unchanged or any(os.path.lexists(sidecar) and os.path.getsize(sidecar) != 0 for sidecar in destination_sidecars):
    os.unlink(destination_path)
    raise SystemExit(1)
for sidecar in destination_sidecars:
    if os.path.lexists(sidecar):
        os.unlink(sidecar)
os.chmod(destination_path, 0o440)
os.chown(destination_path, pwd.getpwnam(user).pw_uid, grp.getgrnam(group).gr_gid)
fd = os.open(destination_path, os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)
directory = os.open(destination_path.parent, os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
PY
}

check_copy_space() {
	sudo -n python3 - "$backup_root" "$@" <<'PY'
import pathlib
import shutil
import sqlite3
import sys
root = pathlib.Path(sys.argv[1])
sources = sys.argv[2:]
required = 0
for source in sources:
    path = pathlib.Path(source)
    if path.is_symlink() or not path.is_file():
        raise SystemExit(1)
    connection = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True)
    try:
        pages = connection.execute("PRAGMA page_count").fetchone()[0]
        page_size = connection.execute("PRAGMA page_size").fetchone()[0]
        if type(pages) is not int or type(page_size) is not int or pages < 0 or page_size < 1:
            raise SystemExit(1)
        required += pages * page_size
    finally:
        connection.close()
free = shutil.disk_usage(root).free
reserve = max(512 * 1024 * 1024, required // 10)
if free < required + reserve:
    print(f"sealed SQLite copies need {required} bytes plus {reserve} bytes of free-space reserve; only {free} bytes are available", file=sys.stderr)
    raise SystemExit(1)
PY
}

registry_copy="$backup_root/platform/registry.db"
auth_copy="$backup_root/platform/auth.db"
check_copy_space "$source_state/registry.db" "$source_state/auth.db" || fail "the platform SQLite copies exceed available sealed-copy space"
copy_sealed "$source_state/registry.db" "$registry_copy" || fail "the platform registry database could not be sealed"
copy_sealed "$source_state/auth.db" "$auth_copy" || fail "the platform authentication database could not be sealed"

organization_output=$(run_service python3 - "$registry_copy" <<'PY'
import re
import sqlite3
import sys
path = sys.argv[1]
db = sqlite3.connect("file:" + path + "?mode=ro&immutable=1", uri=True)
try:
    app_id = db.execute("PRAGMA application_id").fetchone()[0]
    if app_id != 0x44545247:
        raise SystemExit(1)
    rows = db.execute("SELECT id,state,managed FROM organizations ORDER BY id COLLATE BINARY").fetchall()
    if not rows:
        raise SystemExit(1)
    for organization, state, managed in rows:
        if not isinstance(organization, str) or not re.fullmatch(r"org_[A-Za-z0-9_-]{1,60}", organization):
            raise SystemExit(1)
        if state not in ("requested", "allocating", "ready", "failed", "disabled", "deleting", "deleted") or managed not in (0, 1):
            raise SystemExit(1)
        print(organization, state, managed, sep="\t")
finally:
    db.close()
PY
) || fail "the sealed registry copy cannot provide managed organization IDs"
organizations=()
if [ -n "$organization_output" ]; then mapfile -t organizations <<<"$organization_output"; fi

tenant_organizations=()
metrics_database_count=0
source_databases=()
source_metrics_databases=()
declare -A known_orgs=()
for entry in "${organizations[@]}"; do
	IFS=$'\t' read -r organization state managed <<<"$entry"
	[ -n "$organization" ] || continue
	known_orgs["$organization"]=$state
	database="$tenant_root/$organization/hub.db"
	if sudo -n test -e "$database" || sudo -n test -L "$database"; then
		if ! sudo -n test -f "$database" || sudo -n test -L "$database"; then fail "a registered tenant database is not a regular file"; fi
		source_databases+=("$database")
		if [ "$managed" = 1 ]; then tenant_organizations+=("$organization"); fi
		sudo -n install -d -o root -g "$SERVICE_GROUP" -m 0750 "$backup_root/tenants/$organization"
		sudo -n chown "$SERVICE_USER:$SERVICE_GROUP" "$backup_root/tenants/$organization"
		sudo -n chmod 0750 "$backup_root/tenants/$organization"
	elif [ "$managed" = 1 ] && { [ "$state" = ready ] || [ "$state" = disabled ] || [ "$state" = deleting ]; }; then
		fail "a managed allocated organization has no tenant database"
	fi
	metrics_database="$tenant_root/$organization/hub.db.metrics.sqlite"
	if sudo -n test -e "$metrics_database" || sudo -n test -L "$metrics_database"; then
		if ! sudo -n test -f "$metrics_database" || sudo -n test -L "$metrics_database"; then fail "a registered tenant metrics database is not a regular file"; fi
		source_metrics_databases+=("$metrics_database")
		if [ ! -d "$backup_root/tenants/$organization" ]; then
			sudo -n install -d -o root -g "$SERVICE_GROUP" -m 0750 "$backup_root/tenants/$organization"
			sudo -n chown "$SERVICE_USER:$SERVICE_GROUP" "$backup_root/tenants/$organization"
			sudo -n chmod 0750 "$backup_root/tenants/$organization"
		fi
	fi
done
shopt -s nullglob
for database in "$tenant_root"/*/hub.db; do
	organization=$(basename "$(dirname "$database")")
	[ -n "${known_orgs[$organization]:-}" ] || fail "a tenant database is absent from the source registry"
done
for database in "$tenant_root"/*/hub.db.metrics.sqlite; do
	organization=$(basename "$(dirname "$database")")
	registered_organization=${known_orgs[$organization]:-}
	[ -n "$registered_organization" ] || fail "a tenant metrics database is absent from the managed registry allocation"
done
shopt -u nullglob

check_copy_space "${source_databases[@]}" "${source_metrics_databases[@]}" || fail "the tenant SQLite copies exceed available sealed-copy space"
for database in "${source_databases[@]}"; do
	organization=$(basename "$(dirname "$database")")
	copy_sealed "$database" "$backup_root/tenants/$organization/hub.db" || fail "a tenant database could not be sealed"
done
for database in "${source_metrics_databases[@]}"; do
	organization=$(basename "$(dirname "$database")")
	copy_sealed "$database" "$backup_root/tenants/$organization/hub.db.metrics.sqlite" || fail "a tenant metrics database could not be sealed"
	metrics_database_count=$((metrics_database_count + 1))
done

for organization in "${tenant_organizations[@]}"; do
	run_service "$binary" hub import-core-postgres --entry-config "$entry_config" --database-copy "$backup_root/tenants/$organization/hub.db" --organization "$organization" --repository-root "$repository_root" --format json | python3 -c 'import json,sys; result=json.load(sys.stdin); sys.exit(0 if result.get("core_import_complete") is True else 1)' || fail "a tenant import did not report core_import_complete"
done

sudo -n python3 - "$STATE/POSTGRES_CUTOVER" "$commit" "$SERVICE_GROUP" <<'PY' || fail "the cutover marker could not be written atomically"
import grp
import json
import os
import pathlib
import pwd
import sys
import tempfile
path = pathlib.Path(sys.argv[1])
commit = sys.argv[2]
group = grp.getgrnam(sys.argv[3]).gr_gid
fd, temporary = tempfile.mkstemp(prefix=".POSTGRES_CUTOVER.", dir=path.parent)
try:
    with os.fdopen(fd, "w") as marker:
        marker.write(json.dumps({"backend": "postgres", "commit": commit}) + "\n")
        marker.flush()
        os.fsync(marker.fileno())
    os.chmod(temporary, 0o440)
    os.chown(temporary, 0, group)
    os.link(temporary, path)
    os.unlink(temporary)
    directory = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
except BaseException:
    try:
        os.unlink(temporary)
    except FileNotFoundError:
        pass
    raise
PY
marker_written=true
config_switched=true
sudo -n install -o root -g "$SERVICE_GROUP" -m 0640 "$entry_config" "$sqlite_config.next.$$"
sudo -n mv -f "$sqlite_config.next.$$" "$sqlite_config"
release_switched=true
sudo -n ln -s "$candidate_release" "$current_link.next.$$"
sudo -n mv -f "$current_link.next.$$" "$current_link"
postgres_start_attempted=true
sudo -n systemctl start "$SOCKET" || fail "the PostgreSQL Cloud socket did not start"
sudo -n systemctl start "$UNIT" || fail "the PostgreSQL Cloud service did not start"
running_pid=$(systemctl show -p MainPID --value "$UNIT") || fail "the PostgreSQL Cloud process ID is unavailable"
[ "$running_pid" != 0 ] || fail "the PostgreSQL Cloud service has no main process"
running_exe=$(sudo -n readlink "/proc/$running_pid/exe") || fail "the PostgreSQL Cloud executable is unavailable"
[ "$running_exe" = "$candidate_release" ] || fail "the PostgreSQL Cloud service did not start the selected release"

healthy=false
for _ in $(seq 1 300); do
	if systemctl is-active --quiet "$SOCKET" && systemctl is-active --quiet "$UNIT" && check_health "$commit"; then
		healthy=true
		break
	fi
	sleep 2
done
[ "$healthy" = true ] || fail "the PostgreSQL service did not reach verified health"
complete=true
echo "PostgreSQL cutover is serving commit ${commit:0:9}; retained $metrics_database_count tenant metrics copies"
