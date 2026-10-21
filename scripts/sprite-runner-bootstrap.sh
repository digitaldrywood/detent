#!/usr/bin/env bash
# Run inside a Fly Sprite. Enrollment and identity reuse belong to Detent.
set -euo pipefail
set +x

usage() {
    cat <<'EOF'
Usage: sprite-runner-bootstrap.sh [options] < enrollment-command

Options:
  --version TAG          Pinned Detent release (default: v0.117.41)
  --go-mod PATH          Read the required Go toolchain from this go.mod
  --from-source PATH     Build Detent from this checkout instead of the release
  --config PATH          Runner configuration (default: ~/.config/detent-runner/global.yaml)
  --workspace-root PATH  Project checkouts (default: ~/detent-runner)
  --setup-manifest PATH  Reproduce this setup revision; stop before enrollment
                         when this Sprite cannot match it
  --help                 Show this help

Paste the Hub's "detent hub runner register ..." command on standard input.
At a terminal the script prompts without echoing it. The command is parsed
without a shell, and the token reaches Detent through its environment, never
an argument list, file or log. The host --service flag is replaced by a
Sprite Service. When this Sprite is already registered, send empty input to
reuse its identity. Provider login stays manual; the runner manages checkouts.

Before enrollment the script records a credential-free setup revision in
~/.local/lib/detent-sprite-runner/setup-manifest.json: platform, bootstrap
release, pinned toolchain and allowlisted native caches. Tools this setup
cannot reproduce are reported; declare them in the project's
hooks.runner_setup. Provider logins and the runner identity are never part
of it; each new Sprite signs in once in its own console.
EOF
}

die() { printf 'sprite bootstrap: %s\n' "$*" >&2; exit 1; }
value() { [[ $# -ge 2 && -n $2 ]] || die "missing value for $1"; }

setup_manifest_tool() {
    node - setup-manifest "$@" <<'NODE'
const fs = require('node:fs');
const crypto = require('node:crypto');
const [mode, ...args] = process.argv.slice(3);
const canonical = manifest => ({
    schema: manifest.schema,
    platform: manifest.platform,
    bootstrap: manifest.bootstrap,
    toolchain: Object.fromEntries(Object.entries(manifest.toolchain || {}).sort()),
    caches: [...new Set(manifest.caches || [])].sort(),
});
const revision = manifest => crypto.createHash('sha256').update(JSON.stringify(canonical(manifest))).digest('hex');
const read = file => {
    const manifest = JSON.parse(fs.readFileSync(file, 'utf8'));
    if (manifest.schema !== 1) throw new Error('unsupported setup manifest schema');
    return manifest;
};
if (mode === 'configured-root') {
    const path = require('node:path');
    const [yamlModule, configPath, base, home] = args;
    if (fs.existsSync(configPath)) {
        const configured = require(yamlModule).parse(fs.readFileSync(configPath, 'utf8'))?.workspace_root ?? '';
        if (typeof configured !== 'string') throw new Error('workspace_root must be a string');
        if (configured === '~' || configured === '~/') console.log(path.resolve(home));
        else if (configured.startsWith('~/')) console.log(path.resolve(home, configured.slice(2)));
        else if (configured !== '') console.log(path.resolve(base, configured));
    }
} else if (mode === 'canonical') {
    const path = require('node:path');
    for (const candidate of args) {
        let existing = path.resolve(candidate), rest = [];
        while (true) {
            try { existing = fs.realpathSync(existing); break; } catch {}
            const parent = path.dirname(existing);
            if (parent === existing) break;
            rest.unshift(path.basename(existing));
            existing = parent;
        }
        console.log(path.join(existing, ...rest));
    }
} else if (mode === 'field') {
    const value = read(args[0])[args[1]];
    console.log(Array.isArray(value) ? value.join(', ') : value ?? '');
} else if (mode === 'write') {
    const [file, platform, bootstrap, caches, ...rest] = args;
    const split = rest.indexOf('--unreproducible');
    const pairs = split < 0 ? rest : rest.slice(0, split);
    const unreproducible = split < 0 ? [] : rest.slice(split + 1).sort();
    const manifest = canonical({
        schema: 1, platform, bootstrap,
        toolchain: Object.fromEntries(pairs.map(pair => pair.split('=', 2))),
        caches: caches.split(',').filter(Boolean),
    });
    manifest.revision = revision(manifest);
    if (unreproducible.length) manifest.unreproducible = unreproducible;
    fs.writeFileSync(file, JSON.stringify(manifest, null, 2) + '\n', { flag: 'wx' });
} else if (mode === 'compare') {
    const [expected, actual] = args.map(read);
    const want = canonical(expected), got = canonical(actual);
    const differences = [];
    if (expected.revision !== revision(expected)) differences.push('revision');
    for (const key of ['platform', 'bootstrap', 'caches']) {
        if (JSON.stringify(want[key]) !== JSON.stringify(got[key])) differences.push(key);
    }
    for (const tool of new Set([...Object.keys(want.toolchain), ...Object.keys(got.toolchain)])) {
        if (want.toolchain[tool] !== got.toolchain[tool]) differences.push(`toolchain.${tool}`);
    }
    if (differences.length) {
        console.log(differences.join(', '));
        process.exit(1);
    }
} else {
    throw new Error(`unknown setup manifest mode ${mode}`);
}
NODE
}

check_setup_manifest() {
    [[ -f $setup_manifest ]] || die "setup manifest not found: $setup_manifest"
    local platform bootstrap unreproducible
    platform=$(setup_manifest_tool field "$setup_manifest" platform) || die "setup manifest is unreadable"
    bootstrap=$(setup_manifest_tool field "$setup_manifest" bootstrap) || die "setup manifest is unreadable"
    unreproducible=$(setup_manifest_tool field "$setup_manifest" unreproducible) || die "setup manifest is unreadable"
    [[ $platform == "linux/$arch" ]] || die "setup revision targets $platform; this Sprite is linux/$arch"
    [[ $bootstrap == "$version" ]] || die "setup revision needs bootstrap $bootstrap; rerun with --version $bootstrap"
    [[ -z $unreproducible ]] || die "setup revision lists manual customizations it cannot reproduce: $unreproducible; declare them in hooks.runner_setup"
}

setup_customizations() {
    local entry name
    for entry in "$bin_dir"/* "$bin_dir"/.[!.]*; do
        [[ -e $entry || -L $entry ]] || continue
        name=${entry##*/}
        case $name in
            go|gofmt|gh|rg|fd|psql|codex|claude|playwright|detent) ;;
            *) printf 'bin/%s\n' "$name" ;;
        esac
    done
    for entry in "$npm_prefix/lib/node_modules"/[!@.]* "$npm_prefix/lib/node_modules"/@*/*; do
        [[ -d $entry ]] || continue
        name=${entry#"$npm_prefix/lib/node_modules/"}
        case $name in
            @openai/codex|@anthropic-ai/claude-code|playwright|yaml) ;;
            *) printf 'npm/%s\n' "$name" ;;
        esac
    done
    [[ -z $source_root ]] || printf 'detent/source-build\n'
}

setup_path_within() {
    local child=${1%/} parent=${2%/}
    [[ $child == "$parent" || $child == "$parent"/* ]]
}

setup_paths_overlap() {
    setup_path_within "$1" "$2" || setup_path_within "$2" "$1"
}

prepare_setup_cache() {
    local name=$1 path=$2 default=$3 override=$4 marker=$5 excluded configured_root
    local -a canonical=()
    shift 5
    if [[ -z $path || $name == go-build && $path == off ]]; then
        printf '%s=disabled' "$name"
        return
    fi
    [[ $path == /* ]] || die "native cache $name must be an absolute path"
    configured_root=$(setup_manifest_tool configured-root "$npm_prefix/lib/node_modules/yaml" "$config_path" "$workspace_root" "$HOME") ||
        die "cannot read workspace_root from $config_path"
    mapfile -t canonical < <(setup_manifest_tool canonical "$path" "$default" \
        "${CLAUDE_CONFIG_DIR:-$HOME/.claude}" "$HOME"/.claude.json "${CODEX_HOME:-$HOME/.codex}" \
        "${GH_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/gh}" "${XDG_CONFIG_HOME:-$HOME/.config}/git" \
        "${DOCKER_CONFIG:-$HOME/.docker}" "$HOME"/.git-credentials "$HOME"/.netrc "$HOME"/.npmrc "$HOME"/.ssh \
        "$config_path" "$identity_path" "$workspace_root" "${configured_root:-$workspace_root}")
    [[ ${#canonical[@]} -eq 16 ]] || die "could not resolve native cache $name"
    path=${canonical[0]}
    for excluded in "${canonical[@]:2:12}"; do
        if setup_paths_overlap "$path" "$excluded"; then
            die "native cache $name overlaps a credential, identity or job location; nothing was enrolled"
        fi
    done
    for excluded in "${canonical[@]:14}"; do
        if setup_path_within "$excluded" "$path"; then
            die "native cache $name overlaps a credential, identity or job location; nothing was enrolled"
        elif setup_path_within "$path" "$excluded"; then
            override=workspace
        fi
    done
    if [[ ! -e $path && ! -L $path ]] || [[ -d $path && -z $(ls -A -- "$path") ]]; then
        printf '%s=cold' "$name"
    elif [[ ! -d $path ]]; then
        die "native cache $name is not a directory; move it aside and rerun"
    elif [[ -e $path/$marker ]]; then
        printf '%s=warm' "$name"
    elif [[ -n $override || $path != "${canonical[1]}" ]]; then
        printf 'Native cache %s lacks its toolchain marker; operator overrides stay unmanaged\n' "$name" >&2
        printf '%s=unmanaged' "$name"
    elif [[ $# -eq 0 ]]; then
        printf 'Native cache %s lacks its toolchain marker; its toolchain verifies entries and rebuilds invalid ones on use. Nothing was deleted.\n' "$name" >&2
        printf '%s=unverified' "$name"
    else
        printf 'Rebuilding corrupt native cache %s\n' "$name" >&2
        "$@" >&2 || die "could not rebuild native cache $name"
        printf '%s=rebuilt' "$name"
    fi
}

prepare_setup_revision() {
    local docker_state=unavailable manifest="$lib_dir/setup-manifest.json" candidate="$scratch/setup-manifest.json" differences
    local go_build go_modules go_build_state go_modules_state npm_cache npm_state distribution=unknown gopath_default node_version npm_version
    local -a customizations=()
    [[ $docker_status == skipped:* ]] || docker_state=$docker_package_version
    if [[ -r /etc/os-release ]]; then
        distribution=$(. /etc/os-release && printf '%s-%s' "${ID:-linux}" "${VERSION_ID:-rolling}")
    fi
    go_build=$(go env GOCACHE) || die "go env GOCACHE failed"
    go_modules=$(go env GOMODCACHE) || die "go env GOMODCACHE failed"
    gopath_default=${GOPATH:-$HOME/go}
    go_build_state=$(prepare_setup_cache go-build "$go_build" "${XDG_CACHE_HOME:-$HOME/.cache}/go-build" "${GOCACHE:-}" README)
    go_modules_state=$(prepare_setup_cache go-mod "$go_modules" "${gopath_default%%:*}/pkg/mod" "${GOMODCACHE:-}" cache)
    npm_cache=$(npm config get cache) || die "npm config get cache failed"
    npm_state=$(prepare_setup_cache npm "$npm_cache" "$HOME/.npm" "${npm_config_cache:-${NPM_CONFIG_CACHE:-}}" _cacache npm cache verify)
    mapfile -t customizations < <(setup_customizations)
    node_version=$(node --version) || die "node --version failed"
    npm_version=$(npm --version) || die "npm --version failed"
    setup_manifest_tool write "$candidate" "linux/$arch" "$version" go-build,go-mod,npm \
        "go=$go_version" "gh=$gh_version" "rg=$rg_version" "fd=$fd_version" "psql=$psql_package_version" \
        "codex=$codex_version" "claude=$claude_version" "playwright=$playwright_version" "yaml=$yaml_version" \
        "docker=$docker_state" "os=$distribution" "node=${node_version#v}" "npm=$npm_version" --unreproducible "${customizations[@]}"
    if [[ -n $setup_manifest ]]; then
        differences=$(setup_manifest_tool compare "$setup_manifest" "$candidate") ||
            die "this Sprite does not reproduce the setup revision ($differences); nothing was enrolled"
        [[ ${#customizations[@]} -eq 0 ]] ||
            die "this Sprite has manual customizations outside the setup revision: ${customizations[*]}; nothing was enrolled"
    fi
    install -m 644 "$candidate" "$manifest"
    printf 'Setup revision %s recorded in %s (no credentials or runner identity).\n' "$(setup_manifest_tool field "$manifest" revision)" "$manifest"
    printf 'Native caches (optional, rebuilt by their toolchains, never copied between Sprites): %s, %s, %s\n' "$go_build_state" "$go_modules_state" "$npm_state"
    if [[ ${#customizations[@]} -gt 0 ]]; then
        printf 'Manual customizations this setup cannot reproduce; declare them in the project hooks.runner_setup:\n' >&2
        printf '  %s\n' "${customizations[@]}" >&2
    fi
}

version=v0.117.41
go_mod=
source_root=
config_path=
workspace_root=
setup_manifest=
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
while [[ $# -gt 0 ]]; do
    case $1 in
        --version) value "$@"; version=$2; shift 2 ;;
        --go-mod) value "$@"; go_mod=$2; shift 2 ;;
        --from-source) value "$@"; source_root=$2; shift 2 ;;
        --config) value "$@"; config_path=$2; shift 2 ;;
        --workspace-root) value "$@"; workspace_root=$2; shift 2 ;;
        --setup-manifest) value "$@"; setup_manifest=$2; shift 2 ;;
        --help|-h) usage; exit 0 ;;
        *) die "unknown option: $1 (send the enrollment command on standard input)" ;;
    esac
done
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must be a release tag such as v0.117.41"

# Split like a shell for quoting only: no expansion, substitution or operators.
words=()
split_command() {
    local text=$1 word='' quote='' started=false c next i
    for ((i = 0; i < ${#text}; i++)); do
        c=${text:i:1}
        if [[ $quote == "'" ]]; then
            if [[ $c == "'" ]]; then quote=; else word+=$c; fi
        elif [[ $quote == '"' ]]; then
            case $c in
                '"') quote= ;;
                '$'|'`') die "the enrollment command may not contain \$ or backquotes" ;;
                \\)
                    next=${text:i+1:1}
                    case $next in
                        '"'|\\) word+=$next; i=$((i + 1)) ;;
                        $'\n') i=$((i + 1)) ;;
                        *) word+=$c ;;
                    esac ;;
                *) word+=$c ;;
            esac
        else
            case $c in
                ' '|$'\t'|$'\n'|$'\r')
                    if [[ $started == true ]]; then words+=("$word"); word=; started=false; fi ;;
                "'"|'"') quote=$c; started=true ;;
                \\)
                    next=${text:i+1:1}
                    i=$((i + 1))
                    if [[ $next != $'\n' ]]; then word+=$next; started=true; fi ;;
                '$'|'`'|';'|'&'|'|'|'<'|'>'|'('|')') die "the enrollment command may only contain arguments" ;;
                *) word+=$c; started=true ;;
            esac
        fi
    done
    [[ -z $quote ]] || die "the enrollment command has an unterminated quote"
    if [[ $started == true ]]; then words+=("$word"); fi
}

last_completed='none'
scratch=
bootstrap_exit() {
    local status=$?
    if [[ $status -ne 0 ]]; then
        printf '\nBootstrap stopped. Last completed step: %s.\n' "$last_completed" >&2
        printf 'Resume inside this same Sprite: ' >&2
        printf '%q ' bash "${BASH_SOURCE[0]}" --version "$version" >&2
        [[ -z $config_path ]] || printf '%q ' --config "$config_path" >&2
        [[ -z ${requested_workspace_root:-} ]] || printf '%q ' --workspace-root "$requested_workspace_root" >&2
        [[ -z $go_mod ]] || printf '%q ' --go-mod "$go_mod" >&2
        [[ -z $source_root ]] || printf '%q ' --from-source "$source_root" >&2
        [[ -z $setup_manifest ]] || printf '%q ' --setup-manifest "$setup_manifest" >&2
        printf '\nPaste the complete Hub command at the hidden prompt; paste is invisible.\n' >&2
        printf 'Empty input is only for an already registered runner. If enrollment expired, create a fresh Hub command. Keep this Sprite identity and provider sign-ins.\n' >&2
    fi
    [[ -z $scratch ]] || rm -rf -- "$scratch"
}
trap bootstrap_exit EXIT

enrollment=
if [[ -t 0 ]]; then
    printf 'Paste the Hub enrollment command (input hidden; empty to reuse this registration): ' >&2
    while IFS= read -rs line; do
        enrollment+=$line$'\n'
        [[ $line == *\\ ]] || break
    done
    printf '\n' >&2
else
    enrollment=$(cat)
fi
split_command "$enrollment"
unset enrollment

token=
register_args=()
set_path() {
    local current=$1 flag=$2 next=$3
    [[ -z $current || $current == "$next" ]] || die "$flag differs between the options and the enrollment command"
    printf '%s' "$next"
}
if [[ ${#words[@]} -gt 0 ]]; then
    [[ ${#words[@]} -ge 4 && ${words[0]} == detent && ${words[1]} == hub && ${words[2]} == runner && ${words[3]} == register ]] ||
        die "expected a detent hub runner register command"
    set -- "${words[@]:4}"
    while [[ $# -gt 0 ]]; do
        case $1 in
            --service|--service=true|--service=false) shift ;;
            --token) value "$@"; token=$2; shift 2 ;;
            --token=*) token=${1#*=}; shift ;;
            --config) value "$@"; config_path=$(set_path "$config_path" --config "$2"); shift 2 ;;
            --config=*) config_path=$(set_path "$config_path" --config "${1#*=}"); shift ;;
            --workspace-root) value "$@"; workspace_root=$(set_path "$workspace_root" --workspace-root "$2"); shift 2 ;;
            --workspace-root=*) workspace_root=$(set_path "$workspace_root" --workspace-root "${1#*=}"); shift ;;
            --url|--name|--organization|--capacity|--isolation-tier) value "$@"; register_args+=("$1" "$2"); shift 2 ;;
            --url=*|--name=*|--organization=*|--capacity=*|--isolation-tier=*) register_args+=("$1"); shift ;;
            --*) die "unsupported enrollment argument: ${1%%=*}" ;;
            *) die "unexpected value in the enrollment command" ;;
        esac
    done
    [[ -n $token ]] || die "the enrollment command has no --token"
fi
unset words
config_path=${config_path:-$HOME/.config/detent-runner/global.yaml}
requested_workspace_root=$workspace_root
[[ $config_path == /* && ( -z $workspace_root || $workspace_root == /* ) ]] || die "--config and --workspace-root must be absolute paths"
identity_path="$(dirname -- "$config_path")/identity.json"
if [[ -z $token ]] && [[ ! -f $config_path || ! -f $identity_path ]]; then
    die "no enrollment command given and $config_path is not registered yet"
fi

legacy_workspace_root=$HOME/detent-runner
registered_now=
local_root=
if command -v findmnt >/dev/null; then
    read -r local_fstype local_mount < <(findmnt -n -o FSTYPE,TARGET --mountpoint /tmp 2>/dev/null || true) || true
    if [[ ${local_fstype:-} =~ ^(ext4|xfs|btrfs)$ && ${local_mount:-} == /* ]]; then
        local_root=$local_mount/detent-runner
    fi
fi
workspace_root=${workspace_root:-${local_root:+$local_root/work}}
workspace_root=${workspace_root:-$legacy_workspace_root}

[[ $(uname -s) == Linux ]] || die "run this script inside a Fly Sprite (Linux)"
case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture" ;;
esac
command -v sprite-env >/dev/null || die "sprite-env is missing; run inside a Fly Sprite"
for tool in curl tar sha256sum npm node apt-get apt-cache dpkg-query; do
    command -v "$tool" >/dev/null || die "$tool is required (preinstalled in fresh Sprites)"
done
[[ -z $setup_manifest ]] || check_setup_manifest
local_caches=()
link_local_cache() {
    local native=$1 target=$local_root/cache/$2 entry
    local_caches+=("$target")
    if [[ -L $native ]]; then
        [[ $(readlink -- "$native") != "$target" ]] || mkdir -p -- "$target"
        return 0
    fi
    if [[ -e $native && ! -d $native ]]; then
        printf 'Native cache %s already has content; it stays on the persistent disk.\n' "$native" >&2
        return 0
    fi
    if [[ -d $native ]]; then
        for entry in "$native"/* "$native"/.[!.]*; do
            [[ -e $entry && ${entry##*/} != _logs ]] || continue
            printf 'Native cache %s already has content; it stays on the persistent disk.\n' "$native" >&2
            return 0
        done
        rm -rf -- "$native"
    fi
    mkdir -p -- "$target" "$(dirname -- "$native")"
    ln -s -- "$target" "$native"
}
if [[ -n $local_root ]]; then
    native_cache_home=${XDG_CACHE_HOME:-$HOME/.cache}
    go_env_file=${GOENV:-${XDG_CONFIG_HOME:-$HOME/.config}/go/env}
    if [[ -z ${GOCACHE:-} ]] && ! grep -Eqs '^GOCACHE=' "$go_env_file"; then
        link_local_cache "$native_cache_home/go-build" go-build
        link_local_cache "$native_cache_home/detent" detent
    fi
    [[ -n ${GOLANGCI_LINT_CACHE:-} ]] || link_local_cache "$native_cache_home/golangci-lint" golangci-lint
    if [[ -z ${GOMODCACHE:-}${GOPATH:-} ]] && ! grep -Eqs '^(GOMODCACHE|GOPATH)=' "$go_env_file"; then
        link_local_cache "$HOME/go/pkg/mod" go-mod
    fi
    if [[ -z ${npm_config_cache:-}${NPM_CONFIG_CACHE:-} ]] &&
        ! grep -Eqs '^[[:space:]]*cache[[:space:]]*=' "${NPM_CONFIG_USERCONFIG:-$HOME/.npmrc}"; then
        link_local_cache "$HOME/.npm" npm
    fi
fi

umask 077
bin_dir="$HOME/.local/bin"
lib_dir="$HOME/.local/lib/detent-sprite-runner"
mkdir -p "$bin_dir" "$lib_dir"
export PATH="$bin_dir:$PATH"
scratch_root=${TMPDIR:-${TMP:-${TEMP:-"$HOME/.cache/detent-bootstrap"}}}
mkdir -p "$scratch_root"
scratch=$(mktemp -d "$scratch_root/sprite-bootstrap.XXXXXX")
last_completed='enrollment input and Sprite prerequisites checked'

download() { curl --fail --silent --show-error --location "$1" --output "$2"; }
verify() {
    local file=$1 manifest=$2 sum
    sum=$(awk -v name="$file" '$2 == name {print $1}' "$manifest")
    [[ $sum =~ ^[a-fA-F0-9]{64}$ ]] || die "missing or invalid checksum for $file"
    (cd "$scratch" && printf '%s  %s\n' "$sum" "$file" | sha256sum --check --status) || die "checksum mismatch for $file"
}

as_root() {
    if [[ $(id -u) == 0 ]]; then "$@"; else sudo "$@"; fi
}

apt_updated=false
apt_update() {
    if [[ $apt_updated == false ]]; then
        as_root apt-get update
        apt_updated=true
    fi
}
apt_install() {
    apt_update
    as_root apt-get install -y --no-install-recommends --allow-downgrades "$@"
}

apt_update
sandbox_packages=()
for prerequisite in bubblewrap:bwrap:--version socat:socat:-V; do
    IFS=: read -r package tool version_flag <<<"$prerequisite"
    if ! command -v "$tool" >/dev/null || ! "$tool" "$version_flag" >/dev/null 2>&1; then
        sandbox_packages+=("$package")
    fi
done
if [[ ${#sandbox_packages[@]} -gt 0 ]]; then
    apt_install "${sandbox_packages[@]}"
fi
for tool in bwrap socat; do
    command -v "$tool" >/dev/null || die "Codex sandbox prerequisite $tool is missing from PATH after installation"
    tool_version_flag=--version
    [[ $tool != socat ]] || tool_version_flag=-V
    "$tool" "$tool_version_flag" >/dev/null 2>&1 || die "Codex sandbox prerequisite $tool cannot run"
done
psql_package_version=
for psql_major in 18 17; do
    psql_package="postgresql-client-$psql_major"
    psql_package_version=$(LC_ALL=C apt-cache policy "$psql_package" | awk '$1 == "Candidate:" && !found++ {print $2}')
    [[ -z $psql_package_version || $psql_package_version == '(none)' ]] || break
done
[[ $psql_package_version =~ ^[0-9][a-zA-Z0-9.+:~_-]*$ ]] ||
    die "configured APT repositories provide no supported PostgreSQL client (18 or 17); configure repositories matching this distribution"

docker_package_version=
docker_status='skipped: mount/network/PID namespaces or writable cgroups unavailable'
if command -v unshare >/dev/null && as_root unshare --mount --net --pid --fork true >/dev/null 2>&1 &&
    as_root test -w /sys/fs/cgroup; then
    docker_package_version=$(LC_ALL=C apt-cache policy docker.io | awk '$1 == "Candidate:" && !found++ {print $2}')
    [[ $docker_package_version =~ ^[0-9][a-zA-Z0-9.+:~_-]*$ ]] ||
        die "configured APT repositories provide no Docker package; configure repositories matching this distribution"
fi
last_completed='distribution package candidates checked'
printf 'Package compatibility checked: %s=%s; Docker: %s\n' \
    "$psql_package" "$psql_package_version" "${docker_package_version:-$docker_status}"

if [[ -n $source_root ]]; then
    source_root=$(cd -- "$source_root" && pwd)
    [[ -f $source_root/go.mod ]] || die "--from-source needs a Detent checkout"
    [[ -n $go_mod ]] || go_mod="$source_root/go.mod"
elif [[ -z $go_mod && -f $script_root/go.mod ]]; then
    go_mod="$script_root/go.mod"
fi
if [[ -z $go_mod ]]; then
    go_mod="$lib_dir/go-$version.mod"
    if [[ ! -f $go_mod ]]; then
        download "https://raw.githubusercontent.com/digitaldrywood/detent/$version/go.mod" "$scratch/go.mod"
        install -m 644 "$scratch/go.mod" "$go_mod"
    fi
fi
[[ -f $go_mod ]] || die "go.mod not found: $go_mod"
go_version=$(awk '$1 == "toolchain" {sub(/^go/, "", $2); print $2; exit}' "$go_mod")
if [[ -z $go_version ]]; then
    go_version=$(awk '$1 == "go" {print $2; exit}' "$go_mod")
    [[ $go_version =~ ^[0-9]+\.[0-9]+$ ]] && go_version+=.0
fi
[[ $go_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "unsupported Go version in $go_mod"
go_dir="$HOME/.local/share/detent-toolchains/go$go_version"
if [[ ! -x $go_dir/bin/go ]] || [[ $("$go_dir/bin/go" version) != *"go$go_version "* ]]; then
    archive="go${go_version}.linux-${arch}.tar.gz"
    printf 'Installing Go %s from go.mod\n' "$go_version"
    download "https://go.dev/dl/$archive" "$scratch/$archive"
    download "https://dl.google.com/go/$archive.sha256" "$scratch/go.sha256"
    printf '%s  %s\n' "$(cat "$scratch/go.sha256")" "$archive" > "$scratch/go-checksums.txt"
    verify "$archive" "$scratch/go-checksums.txt"
    tar -xzf "$scratch/$archive" -C "$scratch"
    mkdir -p "$(dirname -- "$go_dir")"
    rm -rf -- "$go_dir"
    mv "$scratch/go" "$go_dir"
fi
export PATH="$go_dir/bin:$PATH"
ln -sfn "$go_dir/bin/go" "$bin_dir/go"
ln -sfn "$go_dir/bin/gofmt" "$bin_dir/gofmt"
last_completed='Go toolchain installed'
gh_version=2.101.0
if [[ $(gh --version 2>/dev/null | sed -n '1p' || true) != "gh version $gh_version "* ]]; then
    printf 'Installing GitHub CLI %s\n' "$gh_version"
    archive="gh_${gh_version}_linux_${arch}.tar.gz"
    checksums="gh_${gh_version}_checksums.txt"
    base="https://github.com/cli/cli/releases/download/v$gh_version"
    download "$base/$archive" "$scratch/$archive"
    download "$base/$checksums" "$scratch/$checksums"
    verify "$archive" "$scratch/$checksums"
    tar -xzf "$scratch/$archive" -C "$scratch"
    install -m 755 "$scratch/gh_${gh_version}_linux_${arch}/bin/gh" "$bin_dir/gh"
fi

case $arch in
    amd64)
        rust_target=x86_64-unknown-linux-musl
        fd_checksum=2b6bfaae8c48f12050813c2ffe1884c61ea26e750d803df9c9114550a314cd14 ;;
    arm64)
        rust_target=aarch64-unknown-linux-musl
        fd_checksum=996b9b1366433b211cb3bbedba91c9dbce2431842144d925428ead0adf32020b ;;
esac
rg_version=15.2.0
if [[ $(rg --version 2>/dev/null | sed -n '1p' || true) != "ripgrep $rg_version"* ]]; then
    printf 'Installing ripgrep %s\n' "$rg_version"
    archive="ripgrep-${rg_version}-${rust_target}.tar.gz"
    base="https://github.com/BurntSushi/ripgrep/releases/download/$rg_version"
    download "$base/$archive" "$scratch/$archive"
    download "$base/$archive.sha256" "$scratch/$archive.sha256"
    verify "$archive" "$scratch/$archive.sha256"
    tar -xzf "$scratch/$archive" -C "$scratch"
    install -m 755 "$scratch/ripgrep-${rg_version}-${rust_target}/rg" "$bin_dir/rg"
fi
fd_version=10.3.0
if [[ $(fd --version 2>/dev/null || true) != "fd $fd_version" ]]; then
    printf 'Installing fd %s\n' "$fd_version"
    archive="fd-v${fd_version}-${rust_target}.tar.gz"
    download "https://github.com/sharkdp/fd/releases/download/v$fd_version/$archive" "$scratch/$archive"
    printf '%s  %s\n' "$fd_checksum" "$archive" > "$scratch/fd-checksums.txt"
    verify "$archive" "$scratch/fd-checksums.txt"
    tar -xzf "$scratch/$archive" -C "$scratch"
    install -m 755 "$scratch/fd-v${fd_version}-${rust_target}/fd" "$bin_dir/fd"
fi

last_completed='GitHub CLI, ripgrep and fd installed'

if [[ -L $bin_dir/psql && ! -e $bin_dir/psql ]]; then rm -- "$bin_dir/psql"; fi
psql_path=$(dpkg-query -L "$psql_package" 2>/dev/null | awk '/\/bin\/psql$/ && !found++ {print}' || true)
if [[ $(dpkg-query -W -f='${Status} ${Version}' "$psql_package" 2>/dev/null || true) != "install ok installed $psql_package_version" || ! -x $psql_path ]]; then
    printf 'Installing PostgreSQL client %s\n' "$psql_package_version"
    apt_install --reinstall "$psql_package=$psql_package_version"
fi
psql_path=$(dpkg-query -L "$psql_package" | awk '/\/bin\/psql$/ && !found++ {print}')
[[ $psql_path == /* && -x $psql_path ]] || die "$psql_package did not install an executable psql"
[[ $("$psql_path" --version) == "psql (PostgreSQL) $psql_major."* ]] || die "$psql_package installed an incompatible psql"
ln -sfn -- "$psql_path" "$bin_dir/psql"
last_completed='PostgreSQL client installed and executable verified'

npm_prefix="$HOME/.local/share/detent-runner/npm"
codex_version=0.160.0
claude_version=2.1.289
playwright_version=1.63.0
yaml_version=2.8.3
npm_packages=()
if [[ $("$npm_prefix/bin/codex" --version 2>/dev/null || true) != "codex-cli $codex_version" ]]; then
    npm_packages+=("@openai/codex@$codex_version")
fi
if [[ $("$npm_prefix/bin/claude" --version 2>/dev/null || true) != "$claude_version (Claude Code)" ]]; then
    npm_packages+=("@anthropic-ai/claude-code@$claude_version")
fi
if [[ $("$npm_prefix/bin/playwright" --version 2>/dev/null || true) != "Version $playwright_version" ]]; then
    npm_packages+=("playwright@$playwright_version")
fi
if [[ $(node -p 'require(process.argv[1]).version' "$npm_prefix/lib/node_modules/yaml/package.json" 2>/dev/null || true) != "$yaml_version" ]]; then
    npm_packages+=("yaml@$yaml_version")
fi
if [[ ${#npm_packages[@]} -gt 0 ]]; then
    npm install --global --prefix "$npm_prefix" --allow-scripts=@anthropic-ai/claude-code "${npm_packages[@]}"
fi
ln -sfn "$npm_prefix/bin/codex" "$bin_dir/codex"
ln -sfn "$npm_prefix/bin/claude" "$bin_dir/claude"
ln -sfn "$npm_prefix/bin/playwright" "$bin_dir/playwright"
hash -r
last_completed='agent CLIs and Playwright installed'

chromium_screenshot() {
    node - "$npm_prefix/lib/node_modules/playwright" "$scratch/chromium.png" <<'NODE'
const { chromium } = require(process.argv[2]);
(async () => {
    const browser = await chromium.launch({ headless: true });
    try {
        const page = await browser.newPage();
        await page.setContent('<title>Sprite bootstrap</title><p>Chromium is ready.</p>');
        await page.screenshot({ path: process.argv[3] });
        console.log(`Chromium ${browser.version()}: headless screenshot verified`);
    } finally {
        await browser.close();
    }
})().catch(error => { console.error(error); process.exit(1); });
NODE
}
if ! chromium_screenshot > "$scratch/chromium-status.txt" 2> "$scratch/chromium-error.txt"; then
    printf 'Installing Playwright Chromium and system dependencies\n'
    playwright install --with-deps chromium
    chromium_screenshot > "$scratch/chromium-status.txt"
fi

last_completed='Chromium headless screenshot verified'

if [[ -n $docker_package_version ]]; then
    if [[ $(dpkg-query -W -f='${Status} ${Version}' docker.io 2>/dev/null || true) != "install ok installed $docker_package_version" ]]; then
        printf 'Installing Docker %s\n' "$docker_package_version"
        apt_install "docker.io=$docker_package_version"
    fi
    if ! docker info >/dev/null 2>&1; then
        docker_launcher="$lib_dir/start-docker"
        {
            printf '#!/usr/bin/env bash\nset -euo pipefail\n'
            if [[ $(id -u) != 0 ]]; then printf 'exec sudo '; else printf 'exec '; fi
            printf '%q --data-root %q --group %q\n' "$(command -v dockerd)" "$HOME/.local/share/detent-runner/docker" "$(id -gn)"
        } > "$docker_launcher"
        chmod 700 "$docker_launcher"
        sprite-env services create detent-docker --cmd "$docker_launcher" --dir "$HOME" --no-stream
    fi
    docker_status=$(docker --version)
else
    printf 'Docker %s\n' "$docker_status"
fi

last_completed='Docker installation checked (see capability result)'

install_lock="$HOME/.detent/install.lock"
if [[ -n $source_root ]]; then
    printf 'Building Detent from %s\n' "$source_root"
    (cd "$source_root" && GOBIN="$bin_dir" go install ./cmd/detent)
    if grep -qxF "binary=$bin_dir/detent" "$install_lock" 2>/dev/null; then rm -f "$install_lock"; fi
elif [[ $(detent --version 2>/dev/null || true) != "$version" ]] || ! grep -qxF "binary=$bin_dir/detent" "$install_lock" 2>/dev/null; then
    printf 'Installing pinned Detent %s\n' "$version"
    release=${version#v}
    archive="detent_${release}_linux_${arch}.tar.gz"
    checksums="detent_${release}_checksums.txt"
    base="https://github.com/digitaldrywood/detent/releases/download/$version"
    download "$base/$archive" "$scratch/$archive"
    download "$base/$checksums" "$scratch/$checksums"
    verify "$archive" "$scratch/$checksums"
    tar -xzf "$scratch/$archive" -C "$scratch" detent
    install -m 755 "$scratch/detent" "$bin_dir/detent"
    commit=$("$bin_dir/detent" --format pretty version | sed -n 's/^commit: //p' | head -n 1)
    [[ $commit =~ ^[0-9a-f]{7,64}$ ]] || die "cannot read the installed Detent commit"
    mkdir -p "${install_lock%/*}"
    printf 'binary=%s\nversion=%s\ncommit=%s\ninstalled_at=%s\nsha256=%s\n' "$bin_dir/detent" "$release" "$commit" \
        "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$(sha256sum "$bin_dir/detent" | awk '{print $1}')" > "$install_lock.tmp"
    mv "$install_lock.tmp" "$install_lock"
fi
detent --version
last_completed='Detent installed'
prepare_setup_revision
last_completed='setup revision recorded'

if [[ -n $token ]]; then
    # A valid stored credential reuses the same runner; the token is only redeemed when it is not.
    DETENT_RUNNER_ENROLLMENT_TOKEN=$token detent hub runner register "${register_args[@]}" --config "$config_path" --workspace-root "$workspace_root"
    unset token
    registered_now=1
else
    printf 'Reusing the registered runner in %s\n' "$config_path"
fi

last_completed='runner registration command completed or stored identity reused; verify Hub connection'

migrate_workspace_root=
if [[ -n $local_root && -z $requested_workspace_root ]]; then migrate_workspace_root=$workspace_root; fi
pressure_status=$(node - "$npm_prefix/lib/node_modules/yaml" "$config_path" "$legacy_workspace_root" "$migrate_workspace_root" <<'NODE'
const fs = require('node:fs');
const YAML = require(process.argv[2]);
const configPath = process.argv[3];
const [legacyWorkspaceRoot, migrateWorkspaceRoot] = process.argv.slice(4);
const original = fs.readFileSync(configPath, 'utf8');
const config = YAML.parseDocument(original);
if (config.errors.length) throw config.errors[0];
if (!YAML.isMap(config.contents)) throw new Error('Runner configuration must be a YAML mapping');
let changed = false;
for (const path of [['global'], ['global', 'io'], ['global', 'cpu']]) {
    const value = config.getIn(path, true);
    if (value === undefined || (YAML.isScalar(value) && value.value === null)) {
        config.setIn(path, config.createNode({}));
        changed = true;
    } else if (!YAML.isMap(value)) {
        throw new Error(`${path.join('.')} must be a YAML mapping`);
    }
}
for (const signal of ['io', 'cpu']) {
    const path = ['global', signal, 'degraded_max_concurrent_agents'];
    if (!config.hasIn(path)) {
        config.setIn(path, 1);
        changed = true;
    }
}
if (migrateWorkspaceRoot && config.get('workspace_root') === legacyWorkspaceRoot) {
    config.set('workspace_root', migrateWorkspaceRoot);
    changed = true;
}
if (changed) {
    const temporary = `${configPath}.sprite-bootstrap-${process.pid}`;
    try {
        const mode = fs.statSync(configPath).mode & 0o777;
        fs.writeFileSync(temporary, config.toString(), { mode, flag: 'wx' });
        fs.chmodSync(temporary, mode);
        fs.renameSync(temporary, configPath);
    } finally {
        fs.rmSync(temporary, { force: true });
    }
}
console.log(`Sprite pressure capacity: io=${config.getIn(['global', 'io', 'degraded_max_concurrent_agents'])}, cpu=${config.getIn(['global', 'cpu', 'degraded_max_concurrent_agents'])} (missing floors default to 1; operator values preserved).`);
NODE
)

resolve_base=$workspace_root
if [[ -z $requested_workspace_root && -z $registered_now ]]; then resolve_base=$legacy_workspace_root; fi
resolved_roots=$(node - "$npm_prefix/lib/node_modules/yaml" "$config_path" "$identity_path" "$resolve_base" "$requested_workspace_root" "$HOME" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
try {
    const [yamlModule, configPath, identityPath, base, requested, home] = process.argv.slice(2);
    const config = require(yamlModule).parse(fs.readFileSync(configPath, 'utf8'));
    const configured = config?.workspace_root ?? '';
    if (typeof configured !== 'string') throw new Error();
    let root = path.resolve(base);
    let launchDir = root;
    if (configured === '~' || configured === '~/') {
        root = launchDir = path.resolve(home);
    } else if (configured.startsWith('~/')) {
        root = launchDir = path.resolve(home, configured.slice(2));
    } else if (path.isAbsolute(configured)) {
        root = launchDir = path.resolve(configured);
    } else if (configured !== '') {
        root = path.resolve(base, configured);
    }
    if (requested && root !== path.resolve(requested)) throw new Error();
    const { identity } = JSON.parse(fs.readFileSync(identityPath, 'utf8'));
    if (!identity || (identity.project_ids != null && !Array.isArray(identity.project_ids))) throw new Error();
    for (const id of identity.project_ids ?? []) {
        if (typeof id !== 'string' || !/^prj_[a-zA-Z0-9_-]+$/.test(id)) throw new Error();
        console.error(`New managed checkout for ${id}: ${path.join(root, id)}`);
    }
    console.log(root);
    console.log(launchDir);
} catch {
    console.error('Cannot resolve managed checkout paths from this runner configuration and identity; preserve both files and correct the configuration before resuming.');
    process.exit(1);
}
NODE
)
workspace_root=${resolved_roots%%$'\n'*}
launch_dir=${resolved_roots#*$'\n'}

mkdir -p "$workspace_root/.tmp" "$launch_dir"
cpu_count=$(nproc)
[[ $cpu_count =~ ^[1-9][0-9]*$ ]] || die "could not read the Sprite CPU count"
make_load_limit=$((cpu_count / 4 > 0 ? cpu_count / 4 : 1))
launcher="$lib_dir/start"
{
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'export HOME=%q\nexport PATH=%q\nexport TMPDIR=%q\n' "$HOME" "$PATH" "$workspace_root/.tmp"
    printf 'export MAKEFLAGS=%q\n' "-l$make_load_limit"
    printf 'mkdir -p --'
    printf ' %q' "$workspace_root/.tmp" "$launch_dir" "${local_caches[@]}"
    printf '\n'
    printf 'cd -- %q\n' "$launch_dir"
    printf 'exec %q --config %q --headless\n' "$bin_dir/detent" "$config_path"
} > "$launcher"
chmod 700 "$launcher"
# Creating an existing service keeps its name but not a restart; restart it so
# upgrades and finished manual setup take effect.
existing_service=false
if sprite-env services get detent-runner >/dev/null 2>&1; then existing_service=true; fi
sprite-env services create detent-runner --cmd "$launcher" --dir "$HOME" --no-stream
if [[ $existing_service == true ]]; then
    sprite-env curl -X POST 'http://sprite/v1/services/detent-runner/restart?duration=1s'
fi

last_completed='runner Service configured; verify running state'
printf '\n%s\n' "$pressure_status"
printf 'Sprite resource profile: %s CPUs; runner Make load limit -l%s; workers run at lower CPU priority than the runner.\n' "$cpu_count" "$make_load_limit"
if [[ -n $local_root ]]; then
    printf 'Local disk: checkouts in %s; native caches linked into %s.\n' "$workspace_root" "$local_root/cache"
fi
printf '\nManual setup still required before dispatch:\n'
if ! claude auth status >/dev/null 2>&1; then
    printf '  Claude: run claude auth login, or configure a Sprites Anthropic connector.\n'
fi
if ! codex login status >/dev/null 2>&1; then
    printf '  Codex: run codex login (codex login --device-auth on a headless host), or configure a supported OpenAI connector.\n'
fi
if ! gh auth status >/dev/null 2>&1; then
    printf '  GitHub: run gh auth login --hostname github.com --git-protocol https --web, then gh auth setup-git.\n'
fi
printf '  Managed checkout root: %s\n' "$workspace_root"
printf '  Each new native project checkout: %s/PROJECT_ID (use the exact prj_ ID from the Hub).\n' "$workspace_root"
printf '  The runner clones Cloud-allowed repositories and runs hooks.runner_setup; do not prepare a separate clone.\n'
printf '  Verify Git repository access, checkout setup and reported policy provenance in the Hub; approve policy only if offered.\n'
printf '  Re-run this script with empty input to restart the service, then route a Todo issue.\n'
printf '  After auth and checkout changes, take a new recovery checkpoint with sprite-env checkpoints create.\n'
printf '\nCreating an installation checkpoint; authentication, checkout readiness and a completed task still need verification (save the id).\n'
printf 'It holds this runner identity: restore it only on this Sprite, never as a reusable setup.\n'
chromium_status=$(cat "$scratch/chromium-status.txt")
rm -rf -- "$scratch"
sprite-env checkpoints create --comment "detent runner bootstrap $version (recovery only)"
last_completed='installation checkpoint created'
printf '\nInspect service: sprite-env services get detent-runner\n'
printf 'Restore this same Sprite: sprite-env checkpoints restore CHECKPOINT_ID\n'
printf '\nInstalled runner toolset:\n'
go version
gh --version | sed -n '1p'
rg --version | sed -n '1p'
fd --version
psql --version
codex --version
claude --version
playwright --version
printf '%s\n' "$chromium_status"
printf 'Docker: %s\n' "$docker_status"
printf '\nLast completed step: %s. This is not a ready checkpoint; verify provider/Git access, Hub registration, project setup, policy status, Service health and a real task before saving the ready checkpoint.\n' "$last_completed"
