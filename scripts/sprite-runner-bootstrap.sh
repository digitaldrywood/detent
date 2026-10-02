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

if [[ -n $source_root ]]; then
    source_root=$(cd -- "$source_root" && pwd)
    [[ -f $source_root/go.mod ]] || die "--from-source needs a Detent checkout"
    [[ -n $go_mod ]] || go_mod="$source_root/go.mod"
elif [[ -z $go_mod && -f $script_root/go.mod ]]; then
    go_mod="$script_root/go.mod"
fi
if [[ -z $go_mod ]]; then
    go_mod="$scratch/go.mod"
    download "https://raw.githubusercontent.com/digitaldrywood/detent/$version/go.mod" "$go_mod"
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
if ! gh --version >/dev/null 2>&1; then
    printf 'Installing GitHub CLI\n'
    if [[ $(id -u) == 0 ]]; then
        apt-get update
        apt-get install -y gh
    else
        sudo apt-get update
        sudo apt-get install -y gh
    fi
fi
# The image already owns ~/.local/bin/codex; npm refuses to replace that link.
npm_prefix="$HOME/.local/share/detent-runner/npm"
npm install --global --prefix "$npm_prefix" --allow-scripts=@anthropic-ai/claude-code @openai/codex @anthropic-ai/claude-code
ln -sfn "$npm_prefix/bin/codex" "$bin_dir/codex"
ln -sfn "$npm_prefix/bin/claude" "$bin_dir/claude"
hash -r
codex --version
claude --version

if [[ -n $source_root ]]; then
    printf 'Building Detent from %s\n' "$source_root"
    (cd "$source_root" && GOBIN="$bin_dir" go install ./cmd/detent)
else
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
rm -rf -- "$scratch"
sprite-env checkpoints create --comment "detent runner bootstrap $version"
printf '\nInspect service: sprite-env services get detent-runner\n'
printf 'Restore this same Sprite: sprite-env checkpoints restore CHECKPOINT_ID\n'
