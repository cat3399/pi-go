#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

npm --prefix "$repo_dir/web" install
npm --prefix "$repo_dir/web" run build
mkdir -p "$repo_dir/bin"

cd "$repo_dir"
go build -tags pi_go_webui -o "$repo_dir/bin/pi-go-web" ./cmd/pi-go-web
