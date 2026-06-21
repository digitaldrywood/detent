#!/bin/sh
set -eu

repo="digitaldrywood/detent"
project_name="detent"
module_package="github.com/digitaldrywood/detent/cmd/detent"
api_base="${DETENT_GITHUB_API_BASE:-https://api.github.com/repos/$repo}"
download_base="${DETENT_RELEASE_DOWNLOAD_BASE:-https://github.com/$repo/releases/download}"
state_dir="${DETENT_STATE_DIR:-"$HOME/.detent"}"
lock_path="${DETENT_INSTALL_LOCK:-"$state_dir/install.lock"}"
source_binary="${DETENT_INSTALL_SOURCE:-}"
install_mode="${DETENT_INSTALL_MODE:-auto}"

abort() {
	printf '%s\n' "$1" >&2
	exit 1
}

script_dir() {
	case "$0" in
		/*) path="$0" ;;
		*) path="$(pwd -P)/$0" ;;
	esac

	if [ -f "$path" ]; then
		dirname "$path"
	else
		printf ''
	fi
}

source_checkout_dir() {
	dir="$(script_dir)"
	if [ -n "$dir" ] && [ -f "$dir/go.mod" ] && [ -d "$dir/cmd/detent" ]; then
		printf '%s\n' "$dir"
		return 0
	fi
	return 1
}

choose_install_dir() {
	if [ -n "${DETENT_INSTALL_DIR:-}" ]; then
		printf '%s\n' "$DETENT_INSTALL_DIR"
		return
	fi
	if [ -n "${PREFIX:-}" ]; then
		printf '%s\n' "$PREFIX"
		return
	fi
	if mkdir -p /usr/local/bin 2>/dev/null && [ -w /usr/local/bin ]; then
		printf '%s\n' /usr/local/bin
		return
	fi
	printf '%s\n' "$HOME/.local/bin"
}

detect_target() {
	uname_s="${DETENT_INSTALL_TEST_UNAME_S:-$(uname -s)}"
	uname_m="${DETENT_INSTALL_TEST_UNAME_M:-$(uname -m)}"

	case "$uname_s" in
		Darwin|darwin) os=darwin ;;
		Linux|linux) os=linux ;;
		*) return 1 ;;
	esac

	case "$uname_m" in
		x86_64|amd64) arch=amd64 ;;
		arm64|aarch64) arch=arm64 ;;
		*) return 1 ;;
	esac

	printf '%s %s\n' "$os" "$arch"
}

download_file() {
	curl -fsSL "$1" -o "$2"
}

download_optional_file() {
	curl -fsSL "$1" -o "$2" 2>/dev/null
}

github_api_token() {
	if [ -n "${DETENT_GITHUB_TOKEN:-}" ]; then
		printf '%s\n' "$DETENT_GITHUB_TOKEN"
		return
	fi
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		printf '%s\n' "$GITHUB_TOKEN"
		return
	fi
	printf ''
}

download_api_file() {
	token="$(github_api_token)"
	if [ -n "$token" ]; then
		curl -fsSL \
			-H "Accept: application/vnd.github+json" \
			-H "Authorization: Bearer $token" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			"$1" -o "$2"
		return
	fi
	download_file "$1" "$2"
}

release_tag() {
	if [ -n "${DETENT_VERSION:-}" ]; then
		printf '%s\n' "$DETENT_VERSION"
		return
	fi

	response="$tmp_dir/latest-release.json"
	if ! download_api_file "$api_base/releases/latest" "$response"; then
		return 1
	fi

	tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$response" | head -n 1)"
	if [ -z "$tag" ]; then
		return 1
	fi
	printf '%s\n' "$tag"
}

trim_v() {
	case "$1" in
		v*) printf '%s\n' "${1#v}" ;;
		*) printf '%s\n' "$1" ;;
	esac
}

download_archive() {
	tag="$1"
	version="$2"
	os="$3"
	arch="$4"
	output="$5"

	version_without_v="$(trim_v "$version")"
	asset_name="$project_name"_"$version_without_v"_"$os"_"$arch".tar.gz
	if download_optional_file "$download_base/$tag/$asset_name" "$output"; then
		printf '%s\n' "$asset_name"
		return 0
	fi

	asset_name="$project_name"_"$version"_"$os"_"$arch".tar.gz
	if [ "$version" != "$version_without_v" ] && download_optional_file "$download_base/$tag/$asset_name" "$output"; then
		printf '%s\n' "$asset_name"
		return 0
	fi

	return 1
}

download_checksums() {
	tag="$1"
	version="$2"
	output="$3"
	version_without_v="$(trim_v "$version")"

	checksum_name="$project_name"_"$version_without_v"_checksums.txt
	if download_optional_file "$download_base/$tag/$checksum_name" "$output"; then
		return 0
	fi
	if download_optional_file "$download_base/$tag/checksums.txt" "$output"; then
		return 0
	fi
	return 1
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
		return
	fi
	abort "Cannot verify checksum: sha256sum or shasum is required"
}

expected_checksum() {
	checksums="$1"
	asset_name="$2"

	awk -v name="$asset_name" '
		{
			file = $2
			sub(/^\*/, "", file)
			if (file == name) {
				print $1
				found = 1
				exit
			}
		}
		END {
			if (!found) {
				exit 1
			}
		}
	' "$checksums"
}

verify_checksum() {
	archive="$1"
	checksums="$2"
	asset_name="$3"

	expected="$(expected_checksum "$checksums" "$asset_name")" || abort "Checksum for $asset_name not found"
	actual="$(sha256_file "$archive")"
	if [ "$actual" != "$expected" ]; then
		abort "Checksum mismatch for $asset_name: expected $expected, got $actual"
	fi
	printf 'Verified checksum for %s\n' "$asset_name"
}

install_release() {
	os="$1"
	arch="$2"

	if ! command -v curl >/dev/null 2>&1; then
		printf '%s\n' "curl is not available; falling back to go install" >&2
		return 1
	fi
	if ! command -v tar >/dev/null 2>&1; then
		printf '%s\n' "tar is not available; falling back to go install" >&2
		return 1
	fi

	tag="$(release_tag)" || {
		printf '%s\n' "Could not resolve the latest Detent release; falling back to go install" >&2
		return 1
	}
	version="$tag"
	archive="$tmp_dir/archive.tar.gz"
	checksums="$tmp_dir/checksums.txt"

	asset_name="$(download_archive "$tag" "$version" "$os" "$arch" "$archive")" || {
		printf '%s\n' "No Detent release asset found for $tag $os/$arch; falling back to go install" >&2
		return 1
	}
	download_checksums "$tag" "$version" "$checksums" || abort "Could not download checksums for release $tag"
	verify_checksum "$archive" "$checksums" "$asset_name"

	mkdir -p "$tmp_dir/release"
	tar -xzf "$archive" -C "$tmp_dir/release"
	if [ ! -f "$tmp_dir/release/detent" ]; then
		abort "Release archive $asset_name did not contain detent"
	fi
	cp "$tmp_dir/release/detent" "$tmp_dir/detent"
}

install_go() {
	version="${DETENT_VERSION:-latest}"
	go_bin="$tmp_dir/go-bin"

	command -v go >/dev/null 2>&1 || abort "Cannot install Detent: release asset unavailable and go is not installed"
	mkdir -p "$go_bin"
	GOBIN="$go_bin" go install "$module_package@$version"
	cp "$go_bin/detent" "$tmp_dir/detent"
}

install_local() {
	dir="$(source_checkout_dir)" || {
		abort "Cannot build Detent locally: install.sh is not running from a checkout"
	}

	build_version="$(git -C "$dir" describe --tags --always 2>/dev/null || echo dev)"
	build_commit="$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo none)"
	build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	ldflags="-X main.version=$build_version -X main.commit=$build_commit -X main.date=$build_date"
	(cd "$dir" && go build -ldflags "$ldflags" -o "$tmp_dir/detent" ./cmd/detent)
}

copy_source() {
	cp "$source_binary" "$tmp_dir/detent"
}

install_binary() {
	if command -v install >/dev/null 2>&1; then
		install -m 0755 "$tmp_dir/detent" "$target"
	else
		cp "$tmp_dir/detent" "$target"
		chmod 0755 "$target"
	fi
}

install_release_or_go() {
	if [ -n "$target_os" ] && install_release "$target_os" "$target_arch"; then
		return
	fi
	install_go
}

install_dir="$(choose_install_dir)"
target="$install_dir/detent"

target_info="$(detect_target || true)"
if [ -n "$target_info" ]; then
	target_os="${target_info% *}"
	target_arch="${target_info#* }"
	printf 'Detected target %s/%s\n' "$target_os" "$target_arch"
else
	target_os=""
	target_arch=""
	printf '%s\n' "No supported release target detected; falling back to go install if needed" >&2
fi

mkdir -p "$install_dir" "$state_dir" || abort "Cannot create install or state directory"
if [ ! -w "$install_dir" ]; then
	abort "Install directory is not writable: $install_dir"
fi

if ! (set -C; : > "$lock_path") 2>/dev/null; then
	abort "Detent is already installed on this host: $lock_path"
fi

cleanup_lock=true
cleanup_lock_file() {
	if [ "$cleanup_lock" = true ]; then
		rm -f "$lock_path"
	fi
}
trap cleanup_lock_file EXIT

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/detent-install.XXXXXX")"
cleanup_tmp() {
	rm -rf "$tmp_dir"
}
trap 'cleanup_tmp; cleanup_lock_file' EXIT

if [ -n "$source_binary" ]; then
	copy_source
elif [ "$install_mode" = "local" ]; then
	install_local
elif [ "$install_mode" = "release" ]; then
	install_release_or_go
elif [ "$install_mode" = "auto" ]; then
	if source_checkout_dir >/dev/null 2>&1; then
		install_local
	else
		install_release_or_go
	fi
else
	abort "Unknown DETENT_INSTALL_MODE: $install_mode"
fi

install_binary

{
	printf 'binary=%s\n' "$target"
	printf 'installed_at=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
} > "$lock_path"

cleanup_lock=false
echo "Installed Detent at $target"
case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*) printf 'Add %s to PATH before running detent: export PATH="%s:$PATH"\n' "$install_dir" "$install_dir" ;;
esac
