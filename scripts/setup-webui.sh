#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

npm --prefix "$repo_dir/surface/web/_frontend" install
