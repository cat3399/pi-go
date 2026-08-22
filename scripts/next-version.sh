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

latest_major=-1
latest_minor=-1
latest_patch=-1

while IFS= read -r tag; do
	if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
		continue
	fi

	major=$((10#${BASH_REMATCH[1]}))
	minor=$((10#${BASH_REMATCH[2]}))
	patch=$((10#${BASH_REMATCH[3]}))
	if (( major > latest_major \
		|| (major == latest_major && minor > latest_minor) \
		|| (major == latest_major && minor == latest_minor && patch > latest_patch) )); then
		latest_major=$major
		latest_minor=$minor
		latest_patch=$patch
	fi
done < <(git tag --list)

# The repository started at 0.1.0-dev without historical release tags. Keep
# the first automated release aligned with that public version.
if (( latest_major < 0 )); then
	printf '0.1.0\n'
	exit 0
fi

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
