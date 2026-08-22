#!/usr/bin/env bash
set -euo pipefail

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

if (( latest_major >= 0 )); then
	printf '%d.%d.%d\n' "$latest_major" "$latest_minor" "$latest_patch"
fi
