# pi-go WebUI frontend

This directory contains the browser frontend owned by `surface/web`. Its visual baseline is the sibling
`pi-web` project at commit `a0668ab5077061a1bd074e11949e0a4b7974db2a`; the initial migration copies the
rendering components, responsive layout, theme, CSS, localization and static assets without the Next.js API
implementation or TypeScript Agent runtime.

During development Next provides HMR and proxies `/api/*` to the API-only Go process. In production Node/Next is a
build-time tool only: `pi-go-web` serves the exported assets and implements HTTP/SSE directly in Go.

From the repository root, install dependencies once with `make web-setup`, then use `make web-dev` for the normal
two-process development loop. `make web-build` produces the ignored `out/` export and tagged `bin/pi-go-web`;
`make web-run` runs it without rebuilding. Make targets delegate to `scripts/webui.sh`, which is also directly usable
on systems without Make. The production binary has no Node/Next dependency.
Next handles frontend HMR while the companion Go watcher rebuilds and restarts only the API process after successful
Go changes; a failed Go build leaves the previous API process running.
The leading underscore in `_frontend` is intentional: Go tooling ignores underscore-prefixed directories, so root
`go test ./...` never walks JavaScript dependencies that happen to contain unrelated Go source files. Production
`go:embed` can still include the explicitly named static export without a staging copy or nested Go module.

The authoritative capability matrix is [`../../../docs/WEBUI.md`](../../../docs/WEBUI.md). An imported control is not evidence
that its backend is implemented. Unavailable modules return structured unsupported responses and are recorded in the
capability ledger; frontend capability gating is the next integration step. They must never return demo data.

Development and checks:

```sh
make web-dev WEB_ARGS='--cwd /path/to/project'
make web-check
make web-build
```
