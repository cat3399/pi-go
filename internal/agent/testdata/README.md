# AgentSession workflow fixtures

The Go tests run the scenarios in `upstream_workflow_corpus.json` with a
deterministic provider and tool. Recorded results are in `upstream_workflow_oracle.json`.

The oracle currently contains twelve production workflows: rich/tool/multi-turn/
reopen, mixed-mode queue/clear/abort/settled, retry/Retry-After recovery,
model/thinking/queue controls, retry-delay cancellation and session reuse, manual
compaction, overflow compaction/continue, Runtime reload/new/resume/import/
dispose with active-response settlement, and the in-flight model/thinking/
active-tools snapshot plus resource reload workflow, as well as tree navigation
and Runtime fork replacement, malformed-line/orphan session resume and
continuation, and skill/template request assembly with dynamic image blocking,
thinking budgets, and rich tool-result images. It records provider inputs, every
AgentSession event and its semantic payload, action results, final state/stats,
normalized JSONL entries/tree structure and physical malformed-line placement,
and contexts obtained by reopening persisted sessions. Only timestamps,
generated entry/session IDs, and temporary absolute paths are normalized.

The manual and overflow cases reuse the inputs to test the current handoff
policy: a submitted checkpoint switches windows; an overflow alone does not.

## Reproducing the recorded results (optional)

The snapshot was captured through `createAgentSession()` in the pi revision
recorded in the corpus. Run from that repository root so `tsx` applies its workspace
path mappings. A fresh upstream checkout must first materialize its ignored
model-data build input with `npm run hydrate:model-data`:

```sh
cd /path/to/pi
PI_UPSTREAM_ROOT=/path/to/pi \
  /path/to/pi/node_modules/.bin/tsx \
  /path/to/pi-go/internal/agent/testdata/generate_upstream_workflow_oracle.ts \
  > /tmp/pi-go-upstream-workflow-oracle.json
```

Compare the output with the checked-in snapshot. The ordinary Go tests read the JSON directly.
