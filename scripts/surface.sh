#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
action=${1:-help}
surface=${2:-terminal}

if [ "$#" -ge 2 ]; then
	shift 2
else
	set --
fi

usage() {
	cat <<'EOF'
Usage: make <action> [SURFACE=<surface>] [ARGS='...']

Actions:
  setup    Install surface dependencies
  check    Check a surface
  build    Build an artifact
  dev      Start a development surface
  run      Run an existing production artifact
  doctor   Check the Android toolchain
  devices  List connected Android devices
  e2e-core Run deterministic production/Application/transport E2E tests
  e2e-deepseek
           Run opt-in live DeepSeek E2E tests (requires DEEPSEEK_API_KEY)

Surfaces:
  terminal  CLI and TUI (default), output: bin/pi-go
  web       CLI, TUI, and embedded Web UI, output: bin/pi-go
  gui       Desktop GUI, output: bin/pi-go-gui
  mobile    Android app, output: surface/mobile/bin/pi-go-mobile.apk

Build variables:
  VERSION     Version embedded in the artifact (default: 0.1.0-dev)
  OUTPUT_DIR  Override the artifact output directory

Examples:
  make build
  make build SURFACE=web
  make build SURFACE=terminal VERSION=1.2.3 OUTPUT_DIR=/tmp/pi-go-build
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build SURFACE=web
  make dev SURFACE=web ARGS='--cwd /path/to/project'
  make e2e-core
  DEEPSEEK_API_KEY='...' make e2e-deepseek
EOF
}

require_surface() {
	case "$surface" in
		terminal|web|gui|mobile) ;;
		*)
			printf 'unknown surface: %s\n\n' "$surface" >&2
			usage >&2
			exit 2
			;;
	esac
}

require_no_args() {
	if [ "$#" -ne 0 ]; then
		printf '%s does not accept ARGS for the %s surface\n' "$action" "$surface" >&2
		exit 2
	fi
}

if [ "$action" = "help" ]; then
	usage
	exit 0
fi
require_surface

case "$action:$surface" in
	setup:terminal)
		require_no_args "$@"
		;;
	setup:web)
		exec "$repo_dir/scripts/webui.sh" setup "$@"
		;;
	setup:gui)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/gui" setup
		;;
	setup:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" setup
		;;
	check:terminal)
		require_no_args "$@"
		cd "$repo_dir"
		exec go test ./internal/... ./cmd/pi-go ./surface/tui
		;;
	check:web)
		require_no_args "$@"
		"$repo_dir/scripts/webui.sh" check
		cd "$repo_dir"
		exec go test ./surface/web ./cmd/pi-go
		;;
	check:gui)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/gui" check
		;;
	check:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" check
		;;
	build:terminal)
		require_no_args "$@"
		output_dir=${OUTPUT_DIR:-"$repo_dir/bin"}
		version=${PI_GO_VERSION:-0.1.0-dev}
		mkdir -p "$output_dir"
		cd "$repo_dir"
		exec go build -trimpath -ldflags "-X=main.version=$version" -o "$output_dir/pi-go$(go env GOEXE)" ./cmd/pi-go
		;;
	build:web)
		require_no_args "$@"
		exec "$repo_dir/scripts/webui.sh" build
		;;
	build:gui)
		require_no_args "$@"
		output_dir=${OUTPUT_DIR:-"$repo_dir/bin"}
		exec make -C "$repo_dir/surface/gui" build BIN_DIR="$output_dir" VERSION="${PI_GO_VERSION:-0.1.0-dev}"
		;;
	build:mobile)
		require_no_args "$@"
		output_dir=${OUTPUT_DIR:-"$repo_dir/surface/mobile/bin"}
		exec make -C "$repo_dir/surface/mobile" build BIN_DIR="$output_dir" VERSION="${PI_GO_VERSION:-0.1.0-dev}"
		;;
	dev:terminal)
		cd "$repo_dir"
		exec go run ./cmd/pi-go tui "$@"
		;;
	dev:web)
		exec "$repo_dir/scripts/webui.sh" dev "$@"
		;;
	dev:gui)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/gui" dev
		;;
	dev:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" run BIN_DIR="$repo_dir/surface/mobile/bin"
		;;
	run:terminal)
		exec "$repo_dir/bin/pi-go" "$@"
		;;
	run:web)
		exec "$repo_dir/scripts/webui.sh" run "$@"
		;;
	run:gui)
		exec "$repo_dir/bin/pi-go-gui" "$@"
		;;
	run:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" run BIN_DIR="$repo_dir/surface/mobile/bin"
		;;
	doctor:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" doctor
		;;
	devices:mobile)
		require_no_args "$@"
		exec make -C "$repo_dir/surface/mobile" device-list
		;;
	doctor:*|devices:*)
		printf '%s is only available for SURFACE=mobile\n' "$action" >&2
		exit 2
		;;
	*)
		printf 'unsupported action: %s\n\n' "$action" >&2
		usage >&2
		exit 2
		;;
esac
