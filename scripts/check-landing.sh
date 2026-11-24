set -euo pipefail

if [ "${1:-}" = --build-custom-linter ]; then
    shift
    linter_version=${1:?linter version required}
    heft_version=${2:?Heft version required}
    output=${3:?output path required}
    procs=${4:?process budget required}
    [[ $output == /* ]]

    output_dir=$(dirname "$output")
    mkdir -p "$output_dir"
    build_dir=$(mktemp -d "${TMPDIR:?job scratch required}/detent-linter.XXXXXX")
    staged_binary=
    trap 'rm -rf -- "$build_dir"; rm -f -- "$staged_binary"' EXIT
    staged_binary=$(mktemp "$output_dir/.golangci-lint.XXXXXX")
    cd "$build_dir"
    export GOWORK=off

    go mod init example.test/detent-linter
    go mod edit -require="github.com/golangci/golangci-lint/v2@$linter_version" \
        -require="github.com/digitaldrywood/heft@$heft_version"
    cat > main.go <<'EOF'
package main

import (
	"fmt"
	"os"
	"runtime"

	_ "github.com/digitaldrywood/heft/plugin"
	"github.com/golangci/golangci-lint/v2/pkg/commands"
	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
)

var version string

func main() {
	if err := commands.Execute(commands.BuildInfo{Version: version, GoVersion: runtime.Version()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitcodes.Failure)
	}
}
EOF
    go build -mod=mod -buildvcs=false -p "$procs" -ldflags "-X main.version=${linter_version#v}" -o "$staged_binary" .
    [[ $(go list -m -f '{{.Version}}' github.com/golangci/golangci-lint/v2) == "$linter_version" ]]
    [[ $(go list -m -f '{{.Version}}' github.com/digitaldrywood/heft) == "$heft_version" ]]
    mv -f -- "$staged_binary" "$output"
    exit
fi

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
if ! base=$(git merge-base --all "$base_head" "$head"); then
    printf 'Landing head %s has no merge base with landing target %s; fetch the shared history before validating.\n' "$head" "$base_head" >&2
    exit 1
fi
if [[ $base == *$'\n'* ]]; then
    printf 'Landing head %s has several merge bases with landing target %s; comparing against the target.\n' "$head" "$base_head"
    base=$base_head
fi
printf 'Landing comparison: base=%s head=%s target=%s\n' "$base" "$head" "$base_head"

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
landing_recipe_scripts = {
    "scripts/check-landing.sh", "scripts/check-evidence.sh", "scripts/check-landing.py",
}
landing_recipe_changed = bool(landing_recipe_scripts.intersection(changed))
direct = set()
propagated = set()
lint = set()
prefixes = {}
runner_package = None
for name, package in packages.items():
    directory = Path(package["Dir"]).relative_to(root).as_posix()
    prefix = "" if directory == "." else directory + "/"
    prefixes[name] = prefix
    if directory == "internal/runner":
        runner_package = name
    if landing_recipe_changed and directory == "internal/workspace/validationcmd":
        direct.add(name)
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
                if prefix or relative not in landing_recipe_scripts:
                    propagated.add(name)
for path in changed:
    if path and not path.endswith(".go"):
        owner = max((name for name, prefix in prefixes.items() if path.startswith(prefix)), key=lambda name: len(prefixes[name]), default=None)
        if owner:
            direct.add(owner)
while True:
    dependents = {name for name, package in packages.items() if propagated.intersection(package.get("Imports", []))}
    if dependents.issubset(propagated):
        break
    propagated.update(dependents)
tested = {
    name for name, package in packages.items()
    if propagated.intersection(package.get("TestImports", []) + package.get("XTestImports", []))
}
selected = set(packages) if wide else direct | propagated | tested
if wide:
    print("wide\t./...")
if landing_recipe_changed and runner_package and runner_package not in selected:
    print("recipe-test\t./internal/runner")
for scope, names in (("test", selected), ("lint", set(packages) if wide else lint)):
    for name in sorted(names):
        directory = Path(packages[name]["Dir"]).relative_to(root).as_posix()
        print(scope + "\t" + ("." if directory == "." else "./" + directory))
PY
)
test_packages=()
lint_packages=()
lint_scope=(--new-from-rev="$base" --whole-files)
recipe_test_package=
while IFS=$'\t' read -r scope package; do
    case "$scope" in
        test) test_packages+=("$package") ;;
        lint) lint_packages+=("$package") ;;
        recipe-test) recipe_test_package=$package ;;
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
    printf 'Landing test packages: %s\n' "${test_packages[*]}"
    package_digest=$(python3 scripts/check-landing.py digest "${test_packages[@]}")
    DETENT_LANDING_PACKAGES=$(printf '%s\n' "${test_packages[@]}") check_with_evidence unit-short python3 scripts/check-landing.py tests --package-count="${#test_packages[@]}" --package-sha256="$package_digest" -p "$procs" -short -timeout=90m
fi
if [ -n "$recipe_test_package" ]; then
    check_with_evidence unit-landing-recipe go test -p "$procs" -short -timeout=120s \
        -run '^TestNativeImplementationReviewPrecedesAcceptanceValidator$' "$recipe_test_package"
fi
if [ "${#lint_packages[@]}" -gt 0 ]; then
    if [ -f .custom-gcl.yml ]; then
        make -j1 -f Makefile -f - landing-linter LANDING_LINTER="$linter" TEST_PROCS="$procs" <<'MAKE'
.PHONY: landing-linter
landing-linter:
	@test "$(LANDING_LINTER)" = "$(GOLANGCI_LINT_RUN)"
	@grep -Fqx 'version: $(GOLANGCI_LINT_VERSION)' .custom-gcl.yml
	GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" bash scripts/check-landing.sh --build-custom-linter "$(GOLANGCI_LINT_VERSION)" "$(HEFT_VERSION)" "$(LANDING_LINTER)" "$(TEST_PROCS)"
MAKE
    else
        make -j1 "$linter" TEST_PROCS="$procs"
    fi
    check_with_evidence lint env GOMAXPROCS="$procs" "$linter" run --allow-parallel-runners --concurrency="$procs" --timeout=5m ${lint_scope[@]+"${lint_scope[@]}"} "${lint_packages[@]}"
fi
if [ "$(git rev-parse --verify 'HEAD^{commit}')" != "$head" ]; then
    printf 'Landing head changed during validation.\n' >&2
    exit 1
fi
