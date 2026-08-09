# pi-go WebUI frontend

This directory contains the optional browser surface for `pi-go-web`. Its visual baseline is the sibling
`pi-web` project at commit `a0668ab5077061a1bd074e11949e0a4b7974db2a`; the initial migration copies the
rendering components, responsive layout, theme, CSS, localization and static assets without the Next.js API
implementation or TypeScript Agent runtime.

Node/Next is a build-time frontend tool only. The production `pi-go-web` process serves the exported assets and
implements HTTP/SSE directly in Go.

`out/` is generated locally and ignored by Git. From the repository root, `./scripts/build-webui.sh` installs the locked
frontend dependencies, produces the static export, and builds the optional tagged Go binary at `bin/pi-go-web`.
The resulting binary embeds the export and has no Node/Next runtime dependency.

The authoritative capability matrix is [`../docs/WEBUI.md`](../docs/WEBUI.md). An imported control is not evidence
that its backend is implemented. Unavailable modules return structured unsupported responses and are recorded in the
capability ledger; frontend capability gating is the next integration step. They must never return demo data.

Development checks:

```sh
npm run typecheck
npm run lint
npm run build
```
