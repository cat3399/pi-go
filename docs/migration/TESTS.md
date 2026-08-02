# Test ledger

本表追踪首批 behavior 的上游测试意图。最终状态只使用 `ported`、`strengthened`、
`deferred`、`intentionally-incompatible` 和 `not-applicable`。M-BASE 已有 Go test，但在
独立复审通过前，相关 test intent 仍保持 `deferred`。

固定上游 commit：`a116523434806910336b9de3e38a41aa5860030b`。

## M-BASE

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-BASE-001` | B-BASE-001 | `packages/coding-agent/test/suite/agent-session-prompt.test.ts` — `prompts while idle and records a single text response`，只提供 user normalization/role happy path | `deferred` | B-BASE-001 unit + later agent scenario；不把它误作 finish/usage 裁判 |
| `T-BASE-002` | B-BASE-001/005 | `packages/ai/test/faux-provider.test.ts` — `supports helper blocks for text, thinking, and tool calls` | `deferred` | text 先 port；thinking 在 B-BASE-005 重评 |
| `T-BASE-003` | B-BASE-002 | `packages/agent/test/agent-loop.test.ts` — `should handle tool calls and results` | `deferred` | B-BASE-002 validation + agent scenario；目标 `strengthened` |
| `T-BASE-004` | B-BASE-001 | `packages/ai/test/faux-provider.test.ts` — `registers a custom provider and estimates usage`；`packages/ai/test/total-tokens.test.ts` — `totalTokens field` | `deferred` | checked total 与真实 provider sum；目标 `strengthened` |
| `T-BASE-005` | B-BASE-003 | `packages/ai/test/faux-provider.test.ts` — `streams an exact event order for fixed-size chunks` | `strengthened` | strict state machine 同时覆盖 mixed tool、顺序和 snapshot |
| `T-BASE-006` | B-BASE-004 | `packages/ai/test/faux-provider.test.ts` — `rejects a queued response without a terminal stop reason` | `ported` | pending final 与 protocol error table |
| `T-BASE-007` | B-BASE-004 | `packages/ai/test/openai-responses-terminal-event.test.ts` — `rejects streams that end before a terminal response event` 与 wrapper error-result case | `strengthened` | malformed EOF、duplicate terminal、sticky failure 与 fuzz |
| `T-BASE-008` | B-BASE-001 | 上游宽 `number`/interface 无非法矩阵 | `strengthened` | Go-only 数字域、subset、逐步 overflow、UTF-8 与 terminal matrix |

## M-PROVIDER

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-PROVIDER-001` | B-PROVIDER-001/003 | `packages/ai/test/faux-provider.test.ts` — `consumes queued responses in order and errors when exhausted` | `strengthened` | FIFO、并发分配、request snapshot 与 typed exhaustion |
| `T-PROVIDER-002` | B-PROVIDER-001 | `packages/ai/test/faux-provider.test.ts` — `can replace and append queued responses` | `strengthened` | replace/append 及非法 step 的原子性 |
| `T-PROVIDER-003` | B-PROVIDER-001/003 | `packages/ai/test/faux-provider.test.ts` — `supports async response factories`、`emits an error when a response factory throws` | `strengthened` | lazy factory、returned error、panic、typed cause 与唯一 terminal |
| `T-PROVIDER-004` | B-PROVIDER-002/003 | `packages/ai/test/faux-provider.test.ts` 的 exact-order、explicit error/aborted cases | `deferred` | text/tool/error 已覆盖；thinking/image block 进入对应 slice 后重评 |
| `T-PROVIDER-005` | B-PROVIDER-003 | `packages/ai/test/faux-provider.test.ts` — `supports aborting before the first chunk` 及同文件 mid-block abort cases | `strengthened` | pre/mid-text/factory-time cancel、race 与 terminal cause |
| `T-PROVIDER-006` | — | `packages/ai/test/faux-provider.test.ts` — `unregisters the provider` | `not-applicable` | 只验证 legacy global compat registry；pi-go 使用显式 instance |
| `T-PROVIDER-007` | B-PROVIDER-004 | `packages/ai/test/providers.test.ts` — mixed-API dispatch、missing implementation；`models-runtime.test.ts` — unknown provider error stream | `deferred` | provider runtime dispatch 里程碑 |
| `T-PROVIDER-008` | B-PROVIDER-005 | `packages/ai/test/openai-responses-terminal-event.test.ts` 的 premature EOF、wrapper error 与唯一 terminal cases | `strengthened` | R-PROVIDER-004；另覆盖 dirty EOF、staged terminal/usage、race 与 fuzz |
| `T-PROVIDER-009` | B-PROVIDER-005 | `packages/ai/test/fetch-option.test.ts` — `passes fetch through streamSimple to OpenAI SDK adapters` | `strengthened` | R-PROVIDER-004；显式 HTTP client、request/error/cancel fixture |
| `T-PROVIDER-010` | B-PROVIDER-005 | `packages/ai/test/stream.test.ts` / `OpenAI Responses Provider (gpt-5.4)` — `should complete basic text generation`、`should handle streaming` | `deferred` | 本地 fixture 先完成；真实 credential smoke 仅显式启用，目标 `ported` |
| `T-PROVIDER-011` | B-PROVIDER-002 + 后续真实 tool adapter | `packages/ai/test/openai-responses-partial-json-cleanup.test.ts` — function-call argument cleanup cases | `deferred` | 不属于 text-only B-PROVIDER-005；真实 tool-call slice 重评 |

Prompt cache、multiple model、unregister 以外的 compat/global registry test 不进入 fake v0.1；
每项在相关 behavior 开始时重新分类，不能批量 skip。

## M-AGENT

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-AGENT-001` | B-AGENT-001 | `packages/coding-agent/test/suite/agent-session-prompt.test.ts` — `prompts while idle and records a single text response` | `ported` | single-turn lifecycle scenario |
| `T-AGENT-002` | B-AGENT-002 | `packages/coding-agent/test/suite/agent-session-prompt.test.ts` — `handles a tool call turn and waits for the follow-up LLM response` | `strengthened` | WF-001 因果顺序与 request snapshot |
| `T-AGENT-003` | B-AGENT-001/003 | `packages/agent/test/agent-loop.test.ts` — `should emit events with AgentMessage types`；`packages/agent/test/agent.test.ts` — `emits full lifecycle events for thrown run failures` | `strengthened` | lifecycle、provider contract failure 与唯一 terminal |
| `T-AGENT-004` | B-AGENT-005 | `packages/agent/test/agent.test.ts` — `should await async subscribers before prompt resolves`、`waitForIdle should wait for async subscribers`、`should throw when prompt() called while streaming` | `strengthened` | coordinator settlement、busy clock reservation 与 race |
| `T-AGENT-005` | B-AGENT-004 | `packages/agent/test/agent.test.ts` — `should ignore tool updates after the tool execution settles`；tool failure cases | `strengthened` | error result、late update gate 与真实 Bash invalid args |
| `T-AGENT-006` | B-AGENT-002/005 | `packages/coding-agent/test/suite/regressions/1717-2113-agent-session-event-settlement.test.ts` — persisted message order/tool barrier 两 case；`packages/coding-agent/test/suite/regressions/6363-agent-settled-event.test.ts` — 三个 settlement cases | `strengthened` | single owner、durable successor barrier 与 Abort acceptance race |
| `T-AGENT-007` | B-AGENT-006 | `packages/agent/test/harness/agent-harness.test.ts` 的 save-point、shutdown、queue 和 listener cases | `deferred` | 真实独立 caller 或后续 upstream scope 要求时重评 |
| `T-AGENT-008` | B-AGENT-008 | `packages/agent/src/agent-loop.ts::executeToolCalls*` 上游会形成 error ToolResult 后可能再次调用 cancelled provider | `intentionally-incompatible` | Go scenario 固定四条 transcript、provider call count=1、zero usage 与 settlement |
| `T-AGENT-009` | B-AGENT-009 | `packages/agent/test/agent-loop.test.ts` — `should not execute tool calls from a length-truncated assistant message` | `deferred` | 当前 M-BASE 拒绝 mixed length/tool terminal；不能以 text-only length 代替 |

AgentSession 与低层 Agent 中重复证明相同 invariant 的测试，在 Go 中由一套
contract/scenario suite 覆盖，不复制两套 runtime test。

## M-SESSION

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-SESSION-001` | B-SESSION-002/003 | AgentSession bash persistence test `persists user, assistant, toolResult, and custom messages in order` | `strengthened` | WF-001 四消息 turn、provenance 与 resume |
| `T-SESSION-002` | B-SESSION-001/003 | `packages/agent/test/harness/session-backends.test.ts` — `writes headers and entries and reopens the aggregate` | `strengthened` | coding v3 wire、canonical state 与 rewrite/reopen |
| `T-SESSION-003` | B-SESSION-007 | `packages/agent/test/harness/session-backends.test.ts` — `serializes concurrent appends into one parent chain` | `strengthened` | concurrent chain、alias writer 与 `go test -race` |
| `T-SESSION-004` | B-SESSION-006 | `packages/agent/test/harness/session-backends.test.ts` — `fails loudly for malformed headers and entries` | `strengthened` | line/future/parent/tree matrix |
| `T-SESSION-005` | B-SESSION-006 | coding test `skips malformed lines but keeps valid ones` | `strengthened` | 有意加强为诊断、文件不变并禁止 append |
| `T-SESSION-006` | B-SESSION-006 | `packages/coding-agent/test/session-file-invalid.test.ts` — `prints a friendly error and preserves non-session file content` | `strengthened` | strict parser + application process preservation |
| `T-SESSION-007` | B-SESSION-004/005 | 上游无 byte-prefix unknown round-trip、partial write、atomic create/fsync、poison/commit-unknown test | `strengthened` | fault injection、cancel boundary、byte-prefix 与 poison matrix |
| `T-SESSION-008` | B-SESSION-008 | `packages/coding-agent/test/session-manager/migration.test.ts` 的 v1->v2/idempotent cases | `deferred` | v0.1 v3 reader/writer通过后重评 |
| `T-SESSION-009` | B-SESSION-005/006 | `packages/coding-agent/docs/session-format.md` tree contract 与 `SessionManager._buildIndex/buildSessionPath` physical-tail behavior | `strengthened` | branch-tail/forest root 接受；forward/broken/cycle 拒绝；Open→Append 原 byte prefix 不变 |
| `T-SESSION-010` | B-SESSION-010/011 | `packages/coding-agent/test/session-manager/tree-traversal.test.ts` branch/reset/tree/path/context cases | `ported` | selection+append serialization、forest reopen、tree snapshot、race 与 graph property fuzz；R-SESSION-003 |
| `T-SESSION-011` | B-SESSION-012 | `tree-traversal.test.ts::createBranchedSession` 与 `custom-session-id.test.ts::forkFrom` | `ported` | atomic no-replace、active-source snapshot、commit-unknown poison quarantine、external strict open、source-byte preservation、cancel/publication fault 与 race；R-SESSION-003 |

## M-TOOL

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-TOOL-001` | B-TOOL-001 | `packages/coding-agent/test/tools.test.ts` — `should execute simple commands` | `strengthened` | fake runner、真实 shell 与 immutable input |
| `T-TOOL-002` | B-TOOL-002 | `packages/coding-agent/test/tools.test.ts` — `should handle command errors`、`should throw error when cwd does not exist`、`should handle process spawn errors` | `strengthened` | typed outcome 与 zero-value/failure matrix |
| `T-TOOL-003` | B-TOOL-003 | `packages/coding-agent/test/tools.test.ts` — `should respect timeout`、`should include full output path for truncated timeout and abort errors` | `strengthened` | process-tree、settlement race 与 cancel/timeout precedence |
| `T-TOOL-004` | B-TOOL-004 | `packages/coding-agent/test/tools.test.ts` — UTF-8 chunk 与 trailing-newline truncation cases | `strengthened` | incremental decoder 与 line/byte boundary matrix |
| `T-TOOL-005` | B-TOOL-004 | `packages/coding-agent/test/tools.test.ts` — `should persist full output when truncation happens by line count only` | `strengthened` | private artifact、fault injection 与 path suppression |
| `T-TOOL-006` | B-TOOL-003/005 | 5208 late-output 与 5303 background-pipe regressions | `strengthened` | idle-grace、late isolation、race 与 process integration |
| `T-TOOL-007` | B-TOOL-001 | `packages/coding-agent/src/utils/shell.ts::getShellEnv/getShellConfig` | `strengthened` | inherited/stripped env 与跨平台 shell resolution |

Command prefix、extension reuse、remote BashOperations、renderer 和 direct user bash tests 在
相应产品 behavior 出现前保持 `deferred`，不混入内置 model tool v0.1。

## M-APP 与 WF-001

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-APP-001` | B-APP-001 | `packages/coding-agent/test/print-mode.test.ts` — `emits session_shutdown in text mode` | `strengthened` | exact stdout/stderr、exit 0 与 flush |
| `T-APP-002` | B-APP-002 | `packages/coding-agent/test/print-mode.test.ts` — `emits session_shutdown and returns non-zero on assistant error` | `strengthened` | stderr/exit、durable failure 与 teardown |
| `T-APP-003` | B-APP-003 | 上游 print source 的 SIGTERM/SIGHUP cleanup；无完整 SIGINT/process test | `strengthened` | 三种 signal、重复 signal、settlement 与进程退出 |
| `T-APP-004` | B-APP-004 | `packages/coding-agent/test/session-file-invalid.test.ts` — `prints a friendly error and preserves non-session file content`；无完整新实例 resume E2E | `strengthened` | invalid preservation 与全新进程 restart/resume |
| `T-APP-005` | B-APP-001/002/003 | 上游无同等 fake-backed process seam | `strengthened` | `_test.go` re-exec 调同一 `app.Run`；release binary 无 test factory |
| `T-APP-006` | B-APP-005/007 | `args.test.ts` provider/model/api-key cases；`model-resolver.test.ts` provider-prefixed/custom model cases | `strengthened` | production request/model/exit integration；R-APP-002 |
| `T-APP-007` | B-APP-006 | `models-runtime.test.ts` explicit/stored/ambient 与 wrong-handler cases；`auth-storage.test.ts` read/malformed intent | `strengthened` | 四层 precedence、secret-safe、无 session/network 副作用 matrix；R-APP-002 |
| `T-APP-008` | B-APP-008 | `session-manager.ts` default cwd-encoded directory/new filename；上游缺同等 crash-safe create test | `strengthened` | filename/header ID/time、explicit resume advancing clock；R-APP-002 |

## M-AUTH

| ID | Behavior | 上游 test intent | 当前状态 | 目标与重评条件 |
| --- | --- | --- | --- | --- |
| `T-AUTH-001` | B-AUTH-001 | `auth-storage.test.ts` read/modify/delete/malformed | `strengthened` | unknown record、duplicate/UTF-8/root matrix、malformed preservation；R-AUTH-001 |
| `T-AUTH-002` | B-AUTH-002 | `auth-storage.ts` mode 0600 intent；上游无 atomic durability/Windows ACL evidence | `strengthened` | permission、pre-rename fault/temp cleanup、Windows-only fail-closed suite/cross-compile；R-AUTH-001 |
| `T-AUTH-003` | B-AUTH-003 | `auth-storage.test.ts` concurrent modifications | `strengthened` | same/different Store、local/file wait cancellation、two contending process writers、failure release/merge、`-race`；R-AUTH-001 |
| `T-AUTH-004` | B-AUTH-004 | `runtime-credentials.test.ts`；`models-runtime.test.ts` ownership | `strengthened` | nonpersistent override、四层 precedence、统一 production resolver、不 lower fallback；R-AUTH-001 |
| `T-AUTH-005` | B-AUTH-005 | `resolve-config-value.test.ts` templates/commands | `intentionally-incompatible` | template/fuzz；command side effect rejected until security/process contract exists；R-AUTH-001 |
| `T-WF-002` | WF-002 | OpenAI Responses basic text/stream + print/session intents 分散证明 | `strengthened` | 本地 HTTP/SSE production workflow；真实 credential smoke 单独保留 |
| `T-WF-001` | WF-001 | AgentSession tool-turn + persistence + print tests 分散证明 | `strengthened` | Go 跨模块 process scenario 整体证明 |

JSON print mode 的 error exit、RPC、interactive、真实 provider 和 terminal integration tests
不属于 WF-001 v0.1；进入对应 workflow 时逐项登记。
