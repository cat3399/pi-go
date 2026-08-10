#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
binary="$repo_dir/bin/pi-go-web"

if [ ! -x "$binary" ]; then
	printf '%s\n' "pi-go-web is not built; run ./scripts/build-webui.sh first" >&2
	exit 1
fi

exec "$binary" "$@"
