#!/usr/bin/env bash
set -euo pipefail

bump=${1:-patch}
case "$bump" in
	major|minor|patch) ;;
	*)
		printf 'usage: %s [major|minor|patch]\n' "$0" >&2
		exit 2
		;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
latest_version=$("$script_dir/latest-version.sh")

# The repository started at 0.1.0-dev without historical release tags. Keep
# the first automated release aligned with that public version.
if [[ -z "$latest_version" ]]; then
	printf '0.1.0\n'
	exit 0
fi

IFS=. read -r latest_major latest_minor latest_patch <<< "$latest_version"

case "$bump" in
	major)
		latest_major=$((latest_major + 1))
		latest_minor=0
		latest_patch=0
		;;
	minor)
		latest_minor=$((latest_minor + 1))
		latest_patch=0
		;;
	patch)
		latest_patch=$((latest_patch + 1))
		;;
esac

printf '%d.%d.%d\n' "$latest_major" "$latest_minor" "$latest_patch"
