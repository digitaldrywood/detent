#!/bin/sh
set -eu

repo="digitaldrywood/detent"
project_name="detent"
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
		curl -fsS \
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

version_at_least() {
	awk -v installed="$(trim_v "$1")" -v required="$(trim_v "$2")" 'BEGIN {
		sub(/\+.*/, "", installed)
		sub(/\+.*/, "", required)
		if (installed !~ /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/ || required !~ /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/) exit 1
		split(installed, current, /[.-]/)
		split(required, minimum, /[.-]/)
		for (i = 1; i <= 3; i++) {
			if (current[i] + 0 > minimum[i] + 0) exit 0
			if (current[i] + 0 < minimum[i] + 0) exit 1
		}
		if (installed == required || installed !~ /-/) exit 0
		if (required !~ /-/) exit 1
		sub(/^[0-9]+\.[0-9]+\.[0-9]+-/, "", installed)
		sub(/^[0-9]+\.[0-9]+\.[0-9]+-/, "", required)
		count = split(installed, current, /\./)
		minimum_count = split(required, minimum, /\./)
		for (i = 1; i <= count && i <= minimum_count; i++) {
			if (current[i] == minimum[i]) continue
			current_numeric = current[i] ~ /^[0-9]+$/
			minimum_numeric = minimum[i] ~ /^[0-9]+$/
			if (current_numeric && minimum_numeric) exit !(current[i] + 0 > minimum[i] + 0)
			if (current_numeric != minimum_numeric) exit current_numeric
			exit !(current[i] > minimum[i])
		}
		exit !(count >= minimum_count)
	}'
}

report_update_command() {
	case "$existing_binary" in
		*/Cellar/*|/opt/homebrew/*|/usr/local/Homebrew/*)
			printf '%s\n' 'Update with: brew upgrade digitaldrywood/tap/detent' >&2
			;;
		*)
			printf 'Update with: "%s" update --yes --from-release\n' "$existing_binary" >&2
			printf '%s\n' 'This switches installations managed by go install to a release binary.' >&2
			printf '%s\n' 'If a local board reports insufficient scope: admin, stop it before retrying the update (Ctrl-C for a foreground board; systemctl --user stop detent.service for a systemd user service; sudo systemctl stop detent.service for a system service).' >&2
			;;
	esac
	printf '%s\n' 'Then rerun the enrollment command.' >&2
}

reuse_existing_install() {
	existing_binary=""
	if [ -e "$lock_path" ]; then
		locked_binary="$(sed -n 's/^binary=//p' "$lock_path" | head -n 1)"
		installed_at="$(sed -n 's/^installed_at=//p' "$lock_path" | head -n 1)"
		if [ -z "$locked_binary" ] || [ -z "$installed_at" ]; then
			abort "Another Detent installation is in progress or incomplete: $lock_path. Wait for it to finish, then rerun the enrollment command."
		fi
		existing_binary="$locked_binary"
	fi
	if [ -n "${DETENT_INSTALL_DIR:-${PREFIX:-}}" ]; then
		if [ -x "$target" ]; then existing_binary="$target"; fi
	elif [ -x "$HOME/.local/bin/detent" ]; then
		existing_binary="$HOME/.local/bin/detent"
	elif [ -x /usr/local/bin/detent ]; then
		existing_binary=/usr/local/bin/detent
	elif command -v detent >/dev/null 2>&1; then
		existing_binary="$(command -v detent)"
	fi
	if [ -z "$existing_binary" ]; then return 1; fi
	if [ ! -x "$existing_binary" ]; then
		abort "Installed Detent binary is missing or not executable: $existing_binary. Restore it, or remove the completed install record with rm \"$lock_path\" and rerun the installer."
	fi

	installed_version=""
	if version_output="$("$existing_binary" --format pretty version 2>/dev/null)"; then
		installed_version="$(printf '%s\n' "$version_output" | sed -n 's/^version: //p' | head -n 1)"
	fi
	if [ -z "$installed_version" ]; then
		printf 'Cannot read the version of Detent at %s.\n' "$existing_binary" >&2
		report_update_command
		exit 1
	fi
	required_version="$(release_tag)" || abort "Cannot determine the required Detent release. Retry the enrollment command when the release service is reachable, or run the installer with DETENT_VERSION set to the Hub-required release."
	if ! version_at_least "$installed_version" "$required_version"; then
		printf 'Detent at %s is %s; this installer requires %s or newer.\n' "$existing_binary" "$installed_version" "$required_version" >&2
		report_update_command
		exit 1
	fi
	printf 'Using existing Detent at %s (version %s). No binary was replaced.\n' "$existing_binary" "$installed_version"
	return 0
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
		printf '%s\n' "curl is not available; falling back to prepared source" >&2
		return 1
	fi
	if ! command -v tar >/dev/null 2>&1; then
		printf '%s\n' "tar is not available; falling back to prepared source" >&2
		return 1
	fi

	tag="$(release_tag)" || {
		printf '%s\n' "Could not resolve the latest Detent release; falling back to prepared source" >&2
		return 1
	}
	version="$tag"
	archive="$tmp_dir/archive.tar.gz"
	checksums="$tmp_dir/checksums.txt"

	asset_name="$(download_archive "$tag" "$version" "$os" "$arch" "$archive")" || {
		printf '%s\n' "No Detent release asset found for $tag $os/$arch; falling back to prepared source" >&2
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

install_source() {
	command -v go >/dev/null 2>&1 || abort "Cannot install Detent: release asset unavailable and go is not installed"
	command -v curl >/dev/null 2>&1 || abort "Cannot download prepared Detent source: curl is not installed"
	command -v tar >/dev/null 2>&1 || abort "Cannot unpack prepared Detent source: tar is not installed"
	tag="$(release_tag)" || abort "Could not resolve the Detent source release"
	version="$(trim_v "$tag")"
	asset_name="${project_name}_${version}_source.tar.gz"
	archive="$tmp_dir/source.tar.gz"
	checksums="$tmp_dir/source-checksums.txt"
	download_file "$download_base/$tag/$asset_name" "$archive" || abort "Could not download prepared source for $tag"
	download_checksums "$tag" "$version" "$checksums" || abort "Could not download checksums for source release $tag"
	verify_checksum "$archive" "$checksums" "$asset_name"
	mkdir -p "$tmp_dir/source"
	tar -xzf "$archive" -C "$tmp_dir/source" --strip-components=1
	[ -s "$tmp_dir/source/BUILD_LDFLAGS" ] || abort "Prepared source archive is missing build identity"
	ldflags="$(cat "$tmp_dir/source/BUILD_LDFLAGS")"
	(cd "$tmp_dir/source" && go build -ldflags "$ldflags" -o "$tmp_dir/detent" ./cmd/detent)
}

install_local() {
	dir="$(source_checkout_dir)" || {
		abort "Cannot build Detent locally: install.sh is not running from a checkout"
	}

	if [ -s "$dir/BUILD_LDFLAGS" ]; then
		ldflags="$(cat "$dir/BUILD_LDFLAGS")"
	else
		command -v make >/dev/null 2>&1 || abort "Building a source checkout requires make and Node; Go-only builds use the prepared release source archive"
		(cd "$dir" && make assets)
		build_version="$(git -C "$dir" describe --tags --always 2>/dev/null || echo dev)"
		build_commit="$(git -C "$dir" rev-parse HEAD 2>/dev/null || echo none)"
		build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
		ldflags="-X main.version=$build_version -X main.commit=$build_commit -X main.date=$build_date"
	fi
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

install_release_or_source() {
	if [ -n "$target_os" ] && install_release "$target_os" "$target_arch"; then
		return
	fi
	install_source
}

report_service_restart_required() {
	if command -v systemctl >/dev/null 2>&1 && systemctl --user is-active --quiet detent.service 2>/dev/null; then
		printf '%s\n' "A running systemd user service was not restarted and is still using the previous Detent binary."
		printf '%s\n' "Restart it to load this installation: systemctl --user restart detent.service"
		return
	fi
	if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet detent.service 2>/dev/null; then
		printf '%s\n' "A running systemd service was not restarted and is still using the previous Detent binary."
		printf '%s\n' "Restart it to load this installation: sudo systemctl restart detent.service"
		return
	fi
	if command -v launchctl >/dev/null 2>&1 && command -v id >/dev/null 2>&1; then
		launchd_target="gui/$(id -u)/com.digitaldrywood.detent"
		if launchctl print "$launchd_target" >/dev/null 2>&1; then
			printf '%s\n' "A running launchd service was not restarted and is still using the previous Detent binary."
			printf 'Restart it to load this installation: launchctl kickstart -k %s\n' "$launchd_target"
		fi
	fi
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
	printf '%s\n' "No supported release target detected; falling back to prepared source if needed" >&2
fi

tmp_dir="$(mktemp -d "${TMPDIR:-${TMP:-${TEMP:-/tmp}}}/detent-install.XXXXXX")"
cleanup_tmp() {
	rm -rf "$tmp_dir"
}
trap cleanup_tmp EXIT

if reuse_existing_install; then exit 0; fi

if [ -e "$target" ] || [ -L "$target" ]; then
	abort "Cannot replace the existing file at $target. Move it aside with mv \"$target\" \"$target.previous\", then rerun the installer."
fi

mkdir -p "$install_dir" "$state_dir" || abort "Cannot create install or state directory"
if [ ! -w "$install_dir" ]; then
	abort "Install directory is not writable: $install_dir"
fi

if ! (set -C; : > "$lock_path") 2>/dev/null; then
	abort "Another Detent installation started: $lock_path. Wait for it to finish, then rerun the enrollment command."
fi

cleanup_lock=true
cleanup_lock_file() {
	if [ "$cleanup_lock" = true ]; then
		rm -f "$lock_path"
	fi
}
trap 'cleanup_tmp; cleanup_lock_file' EXIT

if [ -n "$source_binary" ]; then
	copy_source
elif [ "$install_mode" = "local" ]; then
	install_local
elif [ "$install_mode" = "release" ]; then
	install_release_or_source
elif [ "$install_mode" = "auto" ]; then
	if source_checkout_dir >/dev/null 2>&1; then
		install_local
	else
		install_release_or_source
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
report_service_restart_required
case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*) printf 'Add %s to PATH before running detent: export PATH="%s:$PATH"\n' "$install_dir" "$install_dir" ;;
esac
