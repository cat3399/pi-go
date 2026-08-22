#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
	printf 'usage: %s <terminal|web> <version> <output-dir>\n' "$0" >&2
	exit 2
fi

surface=$1
version=$2
output_dir=$3
case "$surface" in
	terminal|web) ;;
	*)
		printf 'unsupported portable surface: %s\n' "$surface" >&2
		exit 2
		;;
esac
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.-]+)?$ ]]; then
	printf 'version must be semantic, got: %s\n' "$version" >&2
	exit 2
fi

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$output_dir"
output_dir=$(CDPATH= cd -- "$output_dir" && pwd)
staging_root="$output_dir/.staging"
mkdir -p "$staging_root"

skip_frontend_build=0
if [[ "$surface" == web ]]; then
	make -C "$repo_dir" setup SURFACE=web
	PI_GO_VERSION="$version" npm --prefix "$repo_dir/surface/web/_frontend" run build
	skip_frontend_build=1
fi

targets=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}
for target in $targets; do
	case "$target" in
		linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
		*)
			printf 'unsupported target: %s\n' "$target" >&2
			exit 2
			;;
	esac

	goos=${target%/*}
	goarch=${target#*/}
	archive_base="pi-go-${surface}_${version}_${goos}_${goarch}"
	binary_dir="$staging_root/$archive_base"
	mkdir -p "$binary_dir"

	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		SKIP_FRONTEND_BUILD="$skip_frontend_build" \
		make -C "$repo_dir" build SURFACE="$surface" VERSION="$version" OUTPUT_DIR="$binary_dir"

	binary_name=pi-go
	if [[ "$goos" == windows ]]; then
		binary_name=pi-go.exe
		(
			cd "$binary_dir"
			zip -q -j "$output_dir/$archive_base.zip" "$binary_name"
		)
	else
		tar -C "$binary_dir" -czf "$output_dir/$archive_base.tar.gz" "$binary_name"
	fi
done
