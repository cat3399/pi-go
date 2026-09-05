# Frozen upstream tool oracle

`upstream_oracle.json` is generated from the pinned upstream pi checkout's
production exports, not from the Go implementation. The corpus and exact Node,
rg, and fd versions are recorded in `upstream_oracle_corpus.json`. Managed-tool
inputs are accepted only when the current `platform-arch` has an explicit
SHA-256 entry; an unregistered platform fails closed instead of trusting the
version banner. The frozen corpus currently registers `darwin-arm64` only;
another platform must add reviewed rg and fd digests before regeneration.

Grep and Find output are produced by their default production operations using
the verified managed binaries. The Find custom-operation probe records only the
pattern forwarded by the definition; it never supplies the frozen result. The
ignore fixture also records pinned fd/rg behavior for ordered negation,
anchoring, directory-only rules, hierarchical scope, tool-specific ignore
files, nested repository boundaries, and fd/rg's distinct ancestor and
non-repository `.gitignore` discovery.

From the pinned upstream checkout, regenerate with:

```sh
cd /path/to/pi
PI_UPSTREAM_ROOT=/path/to/pi \
PI_ORACLE_TOOL_DIR=/path/to/pinned/agent/bin \
/path/to/pi/node_modules/.bin/tsx /path/to/pi-go/internal/tool/testdata/generate_upstream_oracle.ts
```

Running from the upstream root is required so `tsx` applies its checked-in
TypeScript path mappings instead of resolving workspace packages through
unbuilt `dist` entry points.

Review the stdout diff before updating the frozen JSON. The generator creates
fixtures only under the system temporary directory.
