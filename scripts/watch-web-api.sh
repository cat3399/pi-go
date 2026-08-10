#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dev_binary="/tmp/pi-go-web-dev-$$"
server_pid=""

source_fingerprint() {
	cd "$repo_dir"
	git ls-files --cached --others --exclude-standard -- '*.go' go.mod go.sum 'internal/model/catalogdata/*.json' |
		LC_ALL=C sort -u |
		while IFS= read -r source_file; do
			if [ -f "$source_file" ]; then
				cksum "$source_file"
			fi
		done |
		cksum
}

build_server() {
	printf '%s\n' "pi-go-web API: building Go server"
	(
		cd "$repo_dir"
		go build -o "$dev_binary" ./cmd/pi-go-web
	)
}

start_server() {
	"$dev_binary" --api-only "$@" &
	server_pid=$!
}

stop_server() {
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
		server_pid=""
	fi
}

trap stop_server EXIT HUP INT TERM

build_server
fingerprint=$(source_fingerprint)
start_server "$@"

while :; do
	if ! kill -0 "$server_pid" 2>/dev/null; then
		status=0
		wait "$server_pid" || status=$?
		server_pid=""
		exit "$status"
	fi
	sleep 1
	next_fingerprint=$(source_fingerprint)
	if [ "$next_fingerprint" = "$fingerprint" ]; then
		continue
	fi
	fingerprint=$next_fingerprint
	if build_server; then
		stop_server
		start_server "$@"
	else
		printf '%s\n' "pi-go-web API: build failed; keeping the previous server until the next source change" >&2
	fi
done
