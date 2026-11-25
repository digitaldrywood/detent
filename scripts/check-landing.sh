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
export DETENT_TEST_POSTGRES=binary

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
    make -j1 assets generate-docs TEST_PROCS="$procs"
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
    env -u DETENT_API_TOKEN go test -p "$procs" -short -timeout=30m "${test_packages[@]}"
fi
if [ "${#lint_packages[@]}" -gt 0 ]; then
    make -j1 "$linter" TEST_PROCS="$procs"
    env GOMAXPROCS="$procs" "$linter" run --allow-parallel-runners --concurrency="$procs" --timeout=5m ${lint_scope[@]+"${lint_scope[@]}"} "${lint_packages[@]}"
fi
