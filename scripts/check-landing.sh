set -euo pipefail

if [ "${1:-}" != --supervised ]; then
    exec python3 scripts/check-landing.py supervise "$0" "$@"
fi
shift

base_ref=${1:?base ref required}
procs=${2:?process budget required}
linter=${3:?linter path required}
if ! [[ "$procs" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Process budget must be a positive integer.\n' >&2
    exit 1
fi
export GOMAXPROCS=1
GOTOOLCHAIN=$(awk '$1 == "toolchain" {print $2}' go.mod)
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
source scripts/check-evidence.sh

head=$(git rev-parse --verify 'HEAD^{commit}')
base_head=$(git rev-parse --verify "${base_ref}^{commit}")
base=$base_head
printf 'Landing comparison: base=%s head=%s\n' "$base" "$head"

if [ -z "${TMPDIR:-}" ] || [ ! -d "$TMPDIR" ] || [ ! -w "$TMPDIR" ]; then
    printf 'Landing PostgreSQL tests require a writable job TMPDIR.\n' >&2
    exit 1
fi
if ! command -v initdb >/dev/null || ! command -v postgres >/dev/null; then
    if command -v pg_config >/dev/null; then
        if postgres_bin=$(pg_config --bindir) && [ -d "$postgres_bin" ]; then
            export PATH="$postgres_bin:$PATH"
        fi
    fi
fi
for postgres_tool in initdb postgres; do
    if ! command -v "$postgres_tool" >/dev/null; then
        printf 'Landing PostgreSQL tests require %s in the job PATH or pg_config bindir.\n' "$postgres_tool" >&2
        exit 1
    fi
done
export DETENT_TEST_POSTGRES=binary

check_with_evidence sqlite-policy python3 scripts/check-sqlite-policy.py "$base" "$head"
check_with_evidence assets make -j1 assets generate-docs TEST_PROCS="$procs"
check_with_evidence build go build -p "$procs" ./...

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
for name, package in packages.items():
    directory = Path(package["Dir"]).relative_to(root).as_posix()
    prefix = "" if directory == "." else directory + "/"
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
while True:
    dependents = {
        name for name, package in packages.items()
        if propagated.intersection(package.get("Imports", []) + package.get("TestImports", []) + package.get("XTestImports", []))
    }
    if dependents.issubset(propagated):
        break
    propagated.update(dependents)
selected = set(packages) if wide else direct | propagated
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
while IFS= read -r package; do
    if ! [[ " ${test_packages[*]+${test_packages[*]}} " == *" $package "* ]]; then
        test_packages+=("$package")
    fi
done < <(python3 scripts/check-landing.py packages)
sorted_packages=()
while IFS= read -r package; do
    sorted_packages+=("$package")
done < <(printf '%s\n' "${test_packages[@]}" | sort)
test_packages=("${sorted_packages[@]}")
if [ "${#test_packages[@]}" -gt 0 ]; then
    check_with_evidence unit-short python3 scripts/check-landing.py tests -p "$procs" -short -timeout=10m "${test_packages[@]}"
fi
if [ "${#lint_packages[@]}" -gt 0 ]; then
    make -j1 "$linter" TEST_PROCS="$procs"
    check_with_evidence lint env GOMAXPROCS="$procs" "$linter" run --allow-parallel-runners --concurrency="$procs" --timeout=5m ${lint_scope[@]+"${lint_scope[@]}"} "${lint_packages[@]}"
fi
if [ "$(git rev-parse --verify 'HEAD^{commit}')" != "$head" ]; then
    printf 'Landing head changed during validation.\n' >&2
    exit 1
fi
