#!/usr/bin/env bash
# Run inside a Fly Sprite. Enrollment and identity reuse belong to Detent.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: sprite-runner-bootstrap.sh [options] -- detent hub runner register [flags]

Options:
  --version TAG          Pinned Detent release (default: v0.117.7)
  --go-mod PATH          Read the required Go toolchain from this go.mod
  --from-source PATH     Build Detent from this checkout instead of the release
  --help                 Show this help

Paste the Hub enrollment command after --. Its --service flag is replaced by
a Sprite Service. --config and --workspace-root retain their usual meanings
and defaults. Do not paste a shell command string: pass its arguments directly.
Provider login and cloning the printed project repositories are manual steps.
EOF
}

die() { printf 'sprite bootstrap: %s\n' "$*" >&2; exit 1; }
value() { [[ $# -ge 2 && -n $2 ]] || die "missing value for $1"; }

version=v0.117.7
go_mod=
source_root=
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
while [[ $# -gt 0 ]]; do
    case $1 in
        --version) value "$@"; version=$2; shift 2 ;;
        --go-mod) value "$@"; go_mod=$2; shift 2 ;;
        --from-source) value "$@"; source_root=$2; shift 2 ;;
        --help|-h) usage; exit 0 ;;
        --) shift; break ;;
        *) die "unknown bootstrap option: $1 (put enrollment arguments after --)" ;;
    esac
done
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must be a release tag such as v0.117.7"
if [[ ${1:-} == detent ]]; then
    [[ ${2:-} == hub && ${3:-} == runner && ${4:-} == register ]] || die "expected detent hub runner register"
    shift 4
fi
[[ $# -gt 0 ]] || { usage >&2; exit 1; }

config_path="$HOME/.config/detent-runner/global.yaml"
workspace_root="$HOME/detent-runner"
register_args=()
# Pass the enrollment arguments without eval; never persist the single-use token.
while [[ $# -gt 0 ]]; do
    case $1 in
        --service|--service=true|--service=false) shift ;;
        --config|--workspace-root)
            value "$@"
            if [[ $1 == --config ]]; then config_path=$2; else workspace_root=$2; fi
            shift 2 ;;
        --config=*) config_path=${1#*=}; shift ;;
        --workspace-root=*) workspace_root=${1#*=}; shift ;;
        --url|--token|--name|--organization|--capacity)
            value "$@"; register_args+=("$1" "$2"); shift 2 ;;
        --url=*|--token=*|--name=*|--organization=*|--capacity=*)
            register_args+=("$1"); shift ;;
        *) die "unsupported enrollment argument: $1" ;;
    esac
done
[[ $config_path == /* && $workspace_root == /* ]] || die "--config and --workspace-root must be absolute paths"
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
# A standalone Sprite need not have TMPDIR. Use private Sprite-local scratch,
# never a host-wide temporary directory; honor worker-provided scratch first.
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
    # Go archive names require the .0 patch on a minor-only go directive.
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
# Keep npm's package tree separate, then point the user binaries at it.
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
detent hub runner register "${register_args[@]}" --config "$config_path" --workspace-root "$workspace_root"

# The Sprite supervisor owns process lifetime; don't install systemd or daemonize.
mkdir -p "$workspace_root/.tmp"
launcher="$lib_dir/start"
{
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'export HOME=%q\nexport PATH=%q\nexport TMPDIR=%q\n' "$HOME" "$PATH" "$workspace_root/.tmp"
    printf 'cd -- %q\n' "$workspace_root"
    printf 'exec %q --config %q --headless\n' "$bin_dir/detent" "$config_path"
} > "$launcher"
chmod 700 "$launcher"
# PUT reuses the name but leaves an unchanged command running. Restart an
# existing service so upgrades and completed manual setup take effect now.
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
    printf '  Codex: run codex login (codex login --device-auth for a headless host), or configure a supported OpenAI connector.\n'
fi
if ! gh auth status >/dev/null 2>&1; then
    printf '  GitHub: run gh auth login and gh auth setup-git for private clones/pushes.\n'
fi
printf '  Clone each project into the directory printed by register, including WORKFLOW.md and detent.yaml.\n'
printf '  Prepare dependencies and git author identity; approve the observed repository policy in the Hub.\n'
printf '  Verify Hub runner health and route a Todo issue.\n'
printf '  After auth/checkout changes, update the enrolled baseline with sprite-env checkpoints create.\n'
printf '\nCreating the enrolled, clean checkpoint (save the returned id for restore).\n'
# Remove downloaded archives before snapshotting the filesystem.
rm -rf -- "$scratch"
sprite-env checkpoints create
printf '\nInspect service: sprite-env services get detent-runner\n'
printf 'Restore this same Sprite: sprite-env checkpoints restore CHECKPOINT_ID\n'
