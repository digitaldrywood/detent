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
  --help                 Show this help

Paste the Hub's "detent hub runner register ..." command on standard input.
At a terminal the script prompts without echoing it. The command is parsed
without a shell, and the token reaches Detent through its environment, never
an argument list, file or log. The host --service flag is replaced by a
Sprite Service. When this Sprite is already registered, send empty input to
reuse its identity. Provider login and project checkouts stay manual.
EOF
}

die() { printf 'sprite bootstrap: %s\n' "$*" >&2; exit 1; }
value() { [[ $# -ge 2 && -n $2 ]] || die "missing value for $1"; }

version=v0.117.41
go_mod=
source_root=
config_path=
workspace_root=
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
while [[ $# -gt 0 ]]; do
    case $1 in
        --version) value "$@"; version=$2; shift 2 ;;
        --go-mod) value "$@"; go_mod=$2; shift 2 ;;
        --from-source) value "$@"; source_root=$2; shift 2 ;;
        --config) value "$@"; config_path=$2; shift 2 ;;
        --workspace-root) value "$@"; workspace_root=$2; shift 2 ;;
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
            --url|--name|--organization|--capacity) value "$@"; register_args+=("$1" "$2"); shift 2 ;;
            --url=*|--name=*|--organization=*|--capacity=*) register_args+=("$1"); shift ;;
            --*) die "unsupported enrollment argument: ${1%%=*}" ;;
            *) die "unexpected value in the enrollment command" ;;
        esac
    done
    [[ -n $token ]] || die "the enrollment command has no --token"
fi
unset words
config_path=${config_path:-$HOME/.config/detent-runner/global.yaml}
workspace_root=${workspace_root:-$HOME/detent-runner}
[[ $config_path == /* && $workspace_root == /* ]] || die "--config and --workspace-root must be absolute paths"
identity_path="$(dirname -- "$config_path")/identity.json"
if [[ -z $token ]] && [[ ! -f $config_path || ! -f $identity_path ]]; then
    die "no enrollment command given and $config_path is not registered yet"
fi

[[ $(uname -s) == Linux ]] || die "run this script inside a Fly Sprite (Linux)"
case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture" ;;
esac
command -v sprite-env >/dev/null || die "sprite-env is missing; run inside a Fly Sprite"
for tool in curl tar sha256sum npm; do
    command -v "$tool" >/dev/null || die "$tool is required (preinstalled in fresh Sprites)"
done

umask 077
bin_dir="$HOME/.local/bin"
lib_dir="$HOME/.local/lib/detent-sprite-runner"
mkdir -p "$bin_dir" "$lib_dir"
export PATH="$bin_dir:$PATH"
scratch_root=${TMPDIR:-${TMP:-${TEMP:-"$HOME/.cache/detent-bootstrap"}}}
mkdir -p "$scratch_root"
scratch=$(mktemp -d "$scratch_root/sprite-bootstrap.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

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
apt_install() {
    if [[ $apt_updated == false ]]; then
        as_root apt-get update
        apt_updated=true
    fi
    as_root apt-get install -y --no-install-recommends --allow-downgrades "$@"
}

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

psql_package_version=17.10-0ubuntu0.25.10.1
if [[ $(dpkg-query -W -f='${Status} ${Version}' postgresql-client-17 2>/dev/null || true) != "install ok installed $psql_package_version" ]]; then
    printf 'Installing PostgreSQL client %s\n' "$psql_package_version"
    apt_install "postgresql-client-17=$psql_package_version"
fi
ln -sfn /usr/lib/postgresql/17/bin/psql "$bin_dir/psql"

npm_prefix="$HOME/.local/share/detent-runner/npm"
codex_version=0.160.0
claude_version=2.1.289
playwright_version=1.63.0
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
if [[ ${#npm_packages[@]} -gt 0 ]]; then
    npm install --global --prefix "$npm_prefix" --allow-scripts=@anthropic-ai/claude-code "${npm_packages[@]}"
fi
ln -sfn "$npm_prefix/bin/codex" "$bin_dir/codex"
ln -sfn "$npm_prefix/bin/claude" "$bin_dir/claude"
ln -sfn "$npm_prefix/bin/playwright" "$bin_dir/playwright"
hash -r

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

docker_package_version=29.1.3-0ubuntu3~25.10.1
docker_status='skipped: mount/network/PID namespaces or writable cgroups unavailable'
if command -v unshare >/dev/null && as_root unshare --mount --net --pid --fork true >/dev/null 2>&1 &&
    as_root test -w /sys/fs/cgroup; then
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

if [[ -n $source_root ]]; then
    printf 'Building Detent from %s\n' "$source_root"
    (cd "$source_root" && GOBIN="$bin_dir" go install ./cmd/detent)
elif [[ $(detent --version 2>/dev/null || true) != "$version" ]]; then
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
fi
detent --version

if [[ -n $token ]]; then
    # A valid stored credential reuses the same runner; the token is only redeemed when it is not.
    DETENT_RUNNER_ENROLLMENT_TOKEN=$token detent hub runner register "${register_args[@]}" --config "$config_path" --workspace-root "$workspace_root"
    unset token
else
    printf 'Reusing the registered runner in %s\n' "$config_path"
fi

mkdir -p "$workspace_root/.tmp"
launcher="$lib_dir/start"
{
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'export HOME=%q\nexport PATH=%q\nexport TMPDIR=%q\n' "$HOME" "$PATH" "$workspace_root/.tmp"
    printf 'cd -- %q\n' "$workspace_root"
    printf 'exec %q --config %q --headless\n' "$bin_dir/detent" "$config_path"
} > "$launcher"
chmod 700 "$launcher"
# Creating an existing service keeps its name but not a restart; restart it so
# upgrades and finished manual setup take effect.
existing_service=false
if sprite-env services get detent-runner >/dev/null 2>&1; then existing_service=true; fi
sprite-env services create detent-runner --cmd "$launcher" --dir "$workspace_root" --no-stream
if [[ $existing_service == true ]]; then
    sprite-env curl -X POST 'http://sprite/v1/services/detent-runner/restart?duration=1s'
fi

printf '\nManual setup still required before dispatch:\n'
if ! claude auth status >/dev/null 2>&1; then
    printf '  Claude: run claude auth login, or configure a Sprites Anthropic connector.\n'
fi
if ! codex login status >/dev/null 2>&1; then
    printf '  Codex: run codex login (codex login --device-auth on a headless host), or configure a supported OpenAI connector.\n'
fi
if ! gh auth status >/dev/null 2>&1; then
    printf '  GitHub: run gh auth login and gh auth setup-git for private clones and pushes.\n'
fi
printf '  Clone each project into the directory printed by register, including WORKFLOW.md and detent.yaml.\n'
printf '  Prepare dependencies and git author identity; approve the observed repository policy in the Hub.\n'
printf '  Re-run this script with empty input to restart the service, then route a Todo issue.\n'
printf '  After auth and checkout changes, take a new baseline with sprite-env checkpoints create.\n'
printf '\nCreating the enrolled, clean checkpoint (save the returned id for restore).\n'
chromium_status=$(cat "$scratch/chromium-status.txt")
rm -rf -- "$scratch"
sprite-env checkpoints create --comment "detent runner bootstrap $version"
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
