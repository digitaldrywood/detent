set -euo pipefail

base_ref=${1:?base ref required}
procs=${2:?process budget required}
linter=${3:?linter path required}
if ! [[ "$procs" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Process budget must be a positive integer.\n' >&2
    exit 1
fi
export GOMAXPROCS="$procs"
GOTOOLCHAIN=$(awk '$1 == "toolchain" {print $2}' go.mod)
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}

head=$(git rev-parse --verify 'HEAD^{commit}')
base_head=$(git rev-parse --verify "${base_ref}^{commit}")
if ! base=$(git merge-base --all "$base_head" "$head"); then
    printf 'Landing head %s has no merge base with landing target %s; fetch the shared history before validating.\n' "$head" "$base_head" >&2
    exit 1
fi
if [[ $base == *$'\n'* ]]; then
    base=$base_head
fi
printf 'Landing comparison: base=%s head=%s target=%s\n' "$base" "$head" "$base_head"

if ! command -v initdb >/dev/null || ! command -v postgres >/dev/null; then
    if command -v pg_config >/dev/null; then
        if postgres_bin=$(pg_config --bindir) && [ -d "$postgres_bin" ]; then
            export PATH="$postgres_bin:$PATH"
        fi
    fi
fi

pg_sup_pid=""
cleanup_landing_postgres() {
    if [ -n "$pg_sup_pid" ]; then
        kill -TERM "$pg_sup_pid" 2>/dev/null || true
        local deadline=$((SECONDS + 15))
        while kill -0 "$pg_sup_pid" 2>/dev/null && [ "$SECONDS" -lt "$deadline" ]; do
            sleep 0.05
        done
        pg_sup_pid=""
    fi
}
trap cleanup_landing_postgres EXIT

start_landing_postgres() {
    if [ -n "${DETENT_TEST_POSTGRES:-}" ] && [ "${DETENT_TEST_POSTGRES}" != "binary" ]; then
        return 0
    fi
    if ! command -v initdb >/dev/null || ! command -v postgres >/dev/null; then
        export DETENT_TEST_POSTGRES=binary
        return 0
    fi
    local tmp_base="${TMPDIR:-/tmp}"
    local -a pg_extra_args=()
    local pg_major
    pg_major=$(postgres --version | awk '{split($3, a, "."); print a[1]}')
    if [ "${pg_major:-0}" -ge 18 ] 2>/dev/null; then
        pg_extra_args+=(-c file_copy_method=clone)
    fi
    local tag val
    exec 3< <(exec env -u PGHOST -u PGPORT -u PGUSER -u PGPASSWORD -u PGDATABASE -u PGSSLMODE -u PGSSLROOTCERT \
        LC_ALL=C /bin/sh -c '
parent_pid="$1"
tmp_base="$2"
shift 2
sup_pid=$$
printf "SUP %s\n" "$sup_pid"
pg_dir=""
pg_sock=""
child=""
phase="initdb"
watchdog=""
shutting_down=""
cleanup() {
    if [ -n "$watchdog" ]; then
        kill -TERM "$watchdog" 2>/dev/null || true
        wait "$watchdog" 2>/dev/null || true
    fi
    if [ -n "$pg_dir" ]; then
        rm -rf "$pg_dir" 2>/dev/null || true
    fi
    if [ -n "$pg_sock" ]; then
        rm -rf "$pg_sock" 2>/dev/null || true
    fi
}
trap cleanup EXIT
signal_child() {
    if [ -z "$child" ] || [ -z "$shutting_down" ]; then
        return 0
    fi
    if [ "$shutting_down" = "fast" ]; then
        if [ "$phase" = "postgres" ]; then
            kill -INT "$child" 2>/dev/null || true
        else
            kill -TERM "$child" 2>/dev/null || true
        fi
    else
        if [ "$phase" = "postgres" ]; then
            kill -QUIT "$child" 2>/dev/null || true
        else
            kill -KILL "$child" 2>/dev/null || true
        fi
    fi
}
fast_shutdown() {
    shutting_down=fast
    signal_child
}
abort_shutdown() {
    shutting_down=abort
    signal_child
}
trap fast_shutdown TERM INT
trap abort_shutdown USR1
(
    while kill -0 "$sup_pid" 2>/dev/null; do
        ppid=$(ps -o ppid= -p "$sup_pid" 2>/dev/null | tr -d " ")
        if [ -z "$ppid" ] || [ "$ppid" != "$parent_pid" ]; then
            kill -USR1 "$sup_pid" 2>/dev/null || true
            exit 0
        fi
        sleep 0.2
    done
) </dev/null >/dev/null 2>&1 &
watchdog=$!
pg_dir=$(mktemp -d "${tmp_base%/}/detent-landing-pg.XXXXXX") || { printf "ERR mktemp\n"; exit 1; }
pg_sock=$(mktemp -d "/tmp/pgs-land.XXXXXX") || { printf "ERR mktemp\n"; exit 1; }
if [ -z "$pg_dir" ] || [ -z "$pg_sock" ] || [ -n "$shutting_down" ]; then
    printf "ERR mktemp\n"
    exit 1
fi
pg_log="$pg_dir/postgres.log"
initdb -D "$pg_dir/data" -U detent_test --auth=trust --no-locale --encoding=UTF8 </dev/null >"$pg_log" 2>&1 &
child=$!
signal_child
status=0
while kill -0 "$child" 2>/dev/null; do
    wait "$child" && status=0 || status=$?
done
child=""
if [ -n "$shutting_down" ] || [ "$status" -ne 0 ]; then
    cat "$pg_log" >&2 2>/dev/null || true
    printf "ERR initdb\n"
    exit 1
fi
phase="postgres"
postgres \
    -D "$pg_dir/data" \
    -h "" \
    -k "$pg_sock" \
    -c lc_messages=C \
    -c logging_collector=off \
    -c fsync=off \
    -c synchronous_commit=off \
    -c full_page_writes=off \
    -c max_connections=512 \
    "$@" </dev/null >"$pg_log" 2>&1 &
child=$!
signal_child
ready=0
elapsed=0
while [ "$elapsed" -lt 300 ]; do
    if [ -n "$shutting_down" ]; then
        break
    fi
    if [ -f "$pg_log" ] && grep -q "database system is ready to accept connections" "$pg_log"; then
        ready=1
        break
    fi
    if ! kill -0 "$child" 2>/dev/null; then
        break
    fi
    sleep 0.1
    elapsed=$((elapsed + 1))
done
if [ "$ready" -ne 1 ]; then
    cat "$pg_log" >&2 2>/dev/null || true
    kill -QUIT "$child" 2>/dev/null || true
    while kill -0 "$child" 2>/dev/null; do
        wait "$child" || true
    done
    printf "ERR postgres\n"
    exit 1
fi
printf "READY %s %s\n" "$pg_sock" "$pg_dir"
exec >&-
status=0
while kill -0 "$child" 2>/dev/null; do
    wait "$child" && status=0 || status=$?
done
exit "$status"
' pg-landing "$$" "$tmp_base" ${pg_extra_args[@]+"${pg_extra_args[@]}"})
    if read -r tag val <&3 && [ "$tag" = "SUP" ]; then
        pg_sup_pid="$val"
    else
        exec 3<&-
        return 1
    fi
    local pg_sock_ready=""
    if read -r tag pg_sock_ready _ <&3 && [ "$tag" = "READY" ]; then
        exec 3<&-
        export DETENT_TEST_POSTGRES="postgres://detent_test@127.0.0.1:5432/postgres?host=${pg_sock_ready}"
        return 0
    fi
    exec 3<&-
    return 1
}

needs_assets=$(python3 - "$base" "$head" <<'PY'
import os
import subprocess
import sys

base, head = sys.argv[1:]
changed = subprocess.check_output(
    ["git", "diff", "--name-only", "--no-renames", "-z", base, head]
).decode().split("\0")
asset_prefixes = (
    "web/", "static/", "internal/dashboard/", "internal/web/templates/",
    "internal/config/", "internal/policy/", "internal/releasenotes/",
    "tools/releasenotes/", "docs/", "cmd/detent/doc",
)
asset_names = {"Makefile", "package.json", "package-lock.json", "go.mod", "go.sum"}
asset_suffixes = (".templ", "_templ.go", ".css", ".ts", ".tsx", ".js", ".jsx")
if os.path.isfile("web/conversation/package.json") and not (
    os.path.isfile("static/app/conversation/index.html") and os.path.isfile("static/css/output.css")
):
    print("yes")
    sys.exit(0)
if any(
    path and (
        path in asset_names
        or path.startswith(asset_prefixes)
        or path.endswith(asset_suffixes)
    )
    for path in changed
):
    print("yes")
PY
)
if [ "$needs_assets" = "yes" ]; then
    if assets_tail=$(make -j1 assets generate-docs TEST_PROCS="$procs" 2>&1 | tail -c 65536); then
        printf 'Landing assets and generated documentation built successfully.\n'
    else
        assets_status=$?
        printf '%s\n' "$assets_tail" >&2
        exit "$assets_status"
    fi
fi
go build -p "$procs" ./...

selection=$(python3 - "$base" "$head" <<'PY'
import json
from pathlib import Path
import subprocess
import sys

base, head = sys.argv[1:]
changed = subprocess.check_output(
    ["git", "diff", "--name-only", "--no-renames", "-z", base, head]
).decode().split("\0")
raw = subprocess.check_output(["go", "list", "-json", "./..."]).decode()
decoder = json.JSONDecoder()
packages = {}
while raw.strip():
    raw = raw.lstrip()
    package, end = decoder.raw_decode(raw)
    raw = raw[end:]
    packages[package["ImportPath"]] = package
root = Path.cwd()
wide = any(path in ("go.mod", "go.sum") for path in changed)
direct = set()
propagated = set()
lint = set()
prefixes = {}
for name, package in packages.items():
    directory = Path(package["Dir"]).relative_to(root).as_posix()
    prefix = "" if directory == "." else directory + "/"
    prefixes[name] = prefix
    for path in changed:
        if path and path.startswith(prefix):
            relative = path[len(prefix):]
            if relative.endswith(".go") and "/" not in relative:
                direct.add(name)
                lint.add(name)
                if not relative.endswith("_test.go"):
                    propagated.add(name)
            if relative.startswith("testdata/") or relative in package.get("EmbedFiles", []) + package.get("TestEmbedFiles", []) + package.get("XTestEmbedFiles", []):
                direct.add(name)
                propagated.add(name)
for path in changed:
    if path and not path.endswith(".go"):
        owner = max((name for name, prefix in prefixes.items() if path.startswith(prefix)), key=lambda name: len(prefixes[name]), default=None)
        if owner:
            direct.add(owner)
dependents = {
    name for name, package in packages.items()
    if propagated.intersection(package.get("Imports", []) + package.get("TestImports", []) + package.get("XTestImports", []))
}
selected = set(packages) if wide else direct | propagated | dependents
if wide:
    print("wide\t./...")
for scope, names in (("test", selected), ("lint", set(packages) if wide else lint)):
    for name in sorted(names):
        directory = Path(packages[name]["Dir"]).relative_to(root).as_posix()
        print(scope + "\t" + ("." if directory == "." else "./" + directory))
PY
)
test_packages=()
lint_packages=()
lint_scope=(--new-from-rev="$base" --whole-files)
while IFS=$'\t' read -r scope package; do
    case "$scope" in
        test) test_packages+=("$package") ;;
        lint) lint_packages+=("$package") ;;
        wide) lint_scope=() ;;
    esac
done <<< "$selection"
if [ "${#test_packages[@]}" -gt 0 ]; then
    sorted_packages=()
    while IFS= read -r package; do
        sorted_packages+=("$package")
    done < <(printf '%s\n' "${test_packages[@]}" | sort -u)
    test_packages=("${sorted_packages[@]}")
    printf 'Landing test packages: %s\n' "${test_packages[*]}"
    start_landing_postgres
    env -u DETENT_API_TOKEN go test -p "$procs" -short -timeout=30m "${test_packages[@]}"
fi
if [ "${#lint_packages[@]}" -gt 0 ]; then
    make -j1 "$linter" TEST_PROCS="$procs"
    env GOMAXPROCS="$procs" "$linter" run --allow-parallel-runners --concurrency="$procs" --timeout=5m ${lint_scope[@]+"${lint_scope[@]}"} "${lint_packages[@]}"
fi
