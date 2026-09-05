#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
frontend_dir="$repo_dir/surface/web/_frontend"

if [ ! -x "$frontend_dir/node_modules/.bin/next" ]; then
	printf '%s\n' "WebUI dependencies are missing; run ./scripts/setup-webui.sh first" >&2
	exit 1
fi

if [ "${SKIP_FRONTEND_BUILD:-0}" != "1" ]; then
	npm --prefix "$frontend_dir" run build
fi
output_dir=${OUTPUT_DIR:-"$repo_dir/bin"}
version=${PI_GO_VERSION:-0.1.0-dev}
mkdir -p "$output_dir"

cd "$repo_dir"
go build -trimpath -tags pi_go_webui -ldflags "-X=github.com/cat3399/pi-go/internal/product.Version=$version" -o "$output_dir/pi-go$(go env GOEXE)" ./cmd/pi-go
