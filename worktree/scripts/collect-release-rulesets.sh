#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 <owner/repository> <output-path>" >&2
	exit 2
fi

repository="$1"
output="$2"
ruleset_ids="$(mktemp "${output}.ids.XXXXXX")"
rulesets_staged="$(mktemp "${output}.staged.XXXXXX")"

cleanup() {
	rm -f "$ruleset_ids"
	if [[ -n "$rulesets_staged" ]]; then
		rm -f "$rulesets_staged"
	fi
}
trap cleanup EXIT

gh api --paginate \
	"repos/$repository/rulesets?includes_parents=true&per_page=100" \
	--jq '.[] | select(.enforcement == "active" and .target == "branch") | .id' \
	> "$ruleset_ids"

while IFS= read -r ruleset_id; do
	gh api "repos/$repository/rulesets/$ruleset_id" >> "$rulesets_staged"
done < "$ruleset_ids"

mv "$rulesets_staged" "$output"
rulesets_staged=""
