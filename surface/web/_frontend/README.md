# pi-go WebUI frontend

This directory contains the browser frontend owned by `surface/web`. It is a presentation layer above the
Go Application API; it does not own Agent, Runtime, or durable Session state.

The browser uses versioned `/api/v1` command, query, and snapshot endpoints plus one page-wide SSE connection.
Production exports static assets that are embedded into the unified `pi-go` binary. Node.js and Next.js are
build-time and development dependencies only.

Run the normal workflow from the repository root:

```sh
make web-setup
make web-dev WEB_ARGS='--cwd /path/to/project'
make web-check
make web-build
make web-run WEB_ARGS='--cwd /path/to/project'
```

The same operations are available through `./scripts/webui.sh` on systems without Make. The generated `out/`
directory is ignored by Git. The leading underscore in `_frontend` keeps JavaScript dependencies outside Go's
recursive package discovery while still allowing explicit `go:embed` of the static export.

See [`../../../docs/SURFACES.md`](../../../docs/SURFACES.md) for the transport and state-ownership contract.
