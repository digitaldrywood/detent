#!/usr/bin/env bash
set -euo pipefail

gh_command="${DETENT_RELEASE_LOOKUP_GH:-gh}"
sleep_command="${DETENT_RELEASE_LOOKUP_SLEEP:-sleep}"
max_attempts=4
delay=2
max_delay=8
attempt=1

while true; do
	if tag="$("$gh_command" release view --repo "$GITHUB_REPOSITORY" --json tagName --jq .tagName 2>&1)"; then
		if [[ -z "${tag//[[:space:]]/}" ]]; then
			echo "Could not resolve latest Detent release tag: release lookup returned an empty tag" >&2
			exit 1
		fi
		printf '%s\n' "$tag"
		exit 0
	else
		status=$?
	fi

	printf '%s\n' "$tag" >&2
	if [[ ! "$tag" =~ HTTP[[:space:]]+5[0-9][0-9] ]]; then
		echo "Could not resolve latest Detent release tag: permanent release lookup failure" >&2
		exit "$status"
	fi
	if (( attempt >= max_attempts )); then
		echo "Could not resolve latest Detent release tag after $attempt attempts" >&2
		exit "$status"
	fi

	echo "Transient release lookup failure on attempt $attempt; retrying in ${delay}s" >&2
	"$sleep_command" "$delay"
	attempt=$((attempt + 1))
	delay=$((delay * 2))
	if (( delay > max_delay )); then
		delay=$max_delay
	fi
done
