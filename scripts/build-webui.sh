#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
frontend_dir="$repo_dir/surface/web/_frontend"

if [ ! -x "$frontend_dir/node_modules/.bin/next" ]; then
	printf '%s\n' "WebUI dependencies are missing; run ./scripts/setup-webui.sh first" >&2
	exit 1
fi

npm --prefix "$frontend_dir" run build
mkdir -p "$repo_dir/bin"

cd "$repo_dir"
go build -tags pi_go_webui -o "$repo_dir/bin/pi-go" ./cmd/pi-go
