#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
frontend_dir="$repo_dir/surface/web/_frontend"

if [ ! -x "$frontend_dir/node_modules/.bin/next" ]; then
	printf '%s\n' "WebUI dependencies are missing; run ./scripts/setup-webui.sh first" >&2
	exit 1
fi

npm --prefix "$frontend_dir" run build
output_dir=${OUTPUT_DIR:-"$repo_dir/bin"}
mkdir -p "$output_dir"

cd "$repo_dir"
go build -trimpath -tags pi_go_webui -o "$output_dir/pi-go" ./cmd/pi-go
