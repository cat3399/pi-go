#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
frontend_dir="$repo_dir/surface/web/_frontend"
api_listen=${PI_GO_WEB_API_LISTEN:-127.0.0.1:30142}
api_origin=${PI_GO_WEB_API_ORIGIN:-http://127.0.0.1:30142}

if [ ! -x "$frontend_dir/node_modules/.bin/next" ]; then
	printf '%s\n' "WebUI dependencies are missing; run ./scripts/setup-webui.sh first" >&2
	exit 1
fi

api_pid=""
frontend_pid=""

stop_children() {
	if [ -n "$frontend_pid" ]; then
		kill "$frontend_pid" 2>/dev/null || true
	fi
	if [ -n "$api_pid" ]; then
		kill "$api_pid" 2>/dev/null || true
	fi
}

trap stop_children EXIT HUP INT TERM

printf '%s\n' "pi-go-web development UI:  http://127.0.0.1:30141"
printf '%s\n' "pi-go-web development API: $api_origin"

(
	cd "$repo_dir"
	exec "$repo_dir/scripts/watch-web-api.sh" --listen "$api_listen" "$@"
) &
api_pid=$!

(
	cd "$frontend_dir"
	exec env PI_GO_WEB_API_ORIGIN="$api_origin" npm run dev
) &
frontend_pid=$!

while kill -0 "$api_pid" 2>/dev/null && kill -0 "$frontend_pid" 2>/dev/null; do
	sleep 1
done

status=0
if ! kill -0 "$api_pid" 2>/dev/null; then
	wait "$api_pid" || status=$?
else
	wait "$frontend_pid" || status=$?
fi
stop_children
wait "$api_pid" 2>/dev/null || true
wait "$frontend_pid" 2>/dev/null || true
trap - EXIT HUP INT TERM
exit "$status"
