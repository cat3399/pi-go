#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
	printf 'usage: %s <version> <goos-goarch> <output-dir>\n' "$0" >&2
	exit 2
fi

version=$1
target=$2
output_dir=$3
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.-]+)?$ ]]; then
	printf 'version must be semantic, got: %s\n' "$version" >&2
	exit 2
fi

actual_target="$(go env GOOS)-$(go env GOARCH)"
if [[ "$target" != "$actual_target" ]]; then
	printf 'GUI builds must be native: requested %s, runner is %s\n' "$target" "$actual_target" >&2
	exit 2
fi

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
gui_dir="$repo_dir/surface/gui"
mkdir -p "$output_dir"
output_dir=$(CDPATH= cd -- "$output_dir" && pwd)
archive_base="pi-go-gui_${version}_${target/-/_}"
binary_dir="$output_dir/.staging/$archive_base"
mkdir -p "$binary_dir"

npm --prefix "$repo_dir/surface/ui" run check
npm --prefix "$gui_dir/frontend" run build

binary_name=pi-go-gui
if [[ "$(go env GOOS)" == windows ]]; then
	binary_name=pi-go-gui.exe
fi
(
	cd "$gui_dir"
	go build -tags production -trimpath -ldflags "-X=github.com/cat3399/pi-go/internal/product.Version=$version" -o "$binary_dir/$binary_name" .
)

if [[ "$(go env GOOS)" == windows ]]; then
	(
		cd "$binary_dir"
		7z a -bd -tzip "$output_dir/$archive_base.zip" "$binary_name"
	)
else
	tar -C "$binary_dir" -czf "$output_dir/$archive_base.tar.gz" "$binary_name"
fi
