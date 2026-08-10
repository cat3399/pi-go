#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
frontend_dir="$repo_dir/surface/web/_frontend"

usage() {
	cat <<'EOF'
Usage: ./scripts/webui.sh <command> [pi-go-web options]

Commands:
  setup     Install frontend dependencies once
  dev       Start Next HMR and the auto-reloading Go API
  api-dev   Start only the auto-reloading Go API on 127.0.0.1:30142
  check     Run frontend type checking and linting
  test      Run Go Web/TUI surface tests
  build     Build the static frontend and standalone pi-go-web binary
  run       Run the existing standalone pi-go-web binary without rebuilding
EOF
}

command=${1:-}
if [ -z "$command" ] || [ "$command" = "-h" ] || [ "$command" = "--help" ]; then
	usage
	exit 0
fi
shift

case "$command" in
	setup)
		exec "$repo_dir/scripts/setup-webui.sh" "$@"
		;;
	dev)
		exec "$repo_dir/scripts/dev-webui.sh" "$@"
		;;
	api-dev)
		exec "$repo_dir/scripts/watch-web-api.sh" --listen 127.0.0.1:30142 "$@"
		;;
	check)
		if [ "$#" -ne 0 ]; then
			printf '%s\n' "webui check does not accept pi-go-web options" >&2
			exit 2
		fi
		npm --prefix "$frontend_dir" run typecheck
		npm --prefix "$frontend_dir" run lint
		;;
	test)
		if [ "$#" -ne 0 ]; then
			printf '%s\n' "webui test does not accept pi-go-web options" >&2
			exit 2
		fi
		cd "$repo_dir"
		go test ./surface/... ./cmd/pi-go-web
		;;
	build)
		exec "$repo_dir/scripts/build-webui.sh" "$@"
		;;
	run)
		exec "$repo_dir/scripts/run-webui.sh" "$@"
		;;
	*)
		printf 'unknown WebUI command: %s\n\n' "$command" >&2
		usage >&2
		exit 2
		;;
esac
