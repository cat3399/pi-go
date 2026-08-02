# Behavior ledger

本表记录当前已取证的 behavior slice。状态定义见 [README.md](README.md)。固定上游
commit 为 `a116523434806910336b9de3e38a41aa5860030b`。

首个 deterministic standalone workflow 所需的六个领域模块均已通过独立复审；
`WF-001` 已闭环，真实 provider adapter 属于下一里程碑。

## M-BASE

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-BASE-001` | canonical UTF-8 user text；`stop/length` successful text terminal；非负整数 token component、subset 与 checked total 裁判 | `packages/ai/src/types.ts`；`packages/ai/test/total-tokens.test.ts` 的 `totalTokens field`；pi-go strengthened matrix | `ported` | R-BASE-002 |
| `B-BASE-002` | tool call 的 ID/name/raw arguments 与关联 ToolResult 保真且可验证 | `packages/ai/src/types.ts`；`packages/agent/test/agent-loop.test.ts` 的 `should handle tool calls and results` | `ported` | R-BASE-002 |
| `B-BASE-003` | start/text start-delta-end/done 有序、snapshot 不回写、唯一 result | `packages/ai/test/faux-provider.test.ts` 的 `streams an exact event order for fixed-size chunks` | `ported` | R-BASE-002 |
| `B-BASE-004` | error/aborted、unexpected EOF、pending final、duplicate terminal 都不能伪装成功 | `packages/ai/test/faux-provider.test.ts` pending/error cases；`packages/ai/test/openai-responses-terminal-event.test.ts` | `ported` | R-BASE-002 |
| `B-BASE-005` | thinking/image 与受控 vendor replay metadata | `transform-messages.ts` 及 signature/responseId/rawStopReason tests | `deferred` | Responses text adapter 对 reasoning/message phase 显式失败；对应 replay metadata slice 重评 |
| `B-BASE-006` | provider cost 的单位、精度、舍入、total 与非法 raw number policy | `packages/ai/src/types.ts` 只有宽 `number`；首 workflow 无消费证据 | `deferred` | 真实 provider pricing/catalog slice |

## M-PROVIDER

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-PROVIDER-001` | 每实例 FIFO script、固定 chunk/clock/ID/usage、request capture | `packages/ai/test/faux-provider.test.ts` 的 `consumes queued responses in order and errors when exhausted`、`can replace and append queued responses` | `ported` | R-PROVIDER-002 |
| `B-PROVIDER-002` | 一个 tool call 按 block/index 顺序 stream，arguments delta 合成为完整 JSON | `packages/ai/test/faux-provider.test.ts` 的 `streams thinking, text, and partial tool call deltas` 与 exact-order case | `ported` | R-PROVIDER-002；thinking 仍由 B-BASE-005 延后 |
| `B-PROVIDER-003` | queue exhaustion、factory/explicit error、pre/mid cancel 形成唯一 terminal outcome | faux exhaustion/factory/error/abort tests | `ported` | R-PROVIDER-002 |
| `B-PROVIDER-004` | 显式 provider/API dispatch；unknown provider 或缺 adapter 返回 error stream | `providers.test.ts`、`models-runtime.test.ts` | `deferred` | application model装配启动 |
| `B-PROVIDER-005` | 标准 OpenAI Responses 基础 text/SSE 与 terminal handling | `openai-responses-shared.ts` 及 terminal-event tests | `ported` | R-PROVIDER-004；真实 credential smoke 与 production assembler 分开验收 |

## M-AGENT

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-AGENT-001` | idle prompt 的单轮 stream、message/turn/run lifecycle 和 terminal assistant | `packages/coding-agent/test/suite/agent-session-prompt.test.ts` 的 `prompts while idle and records a single text response`；`packages/agent/test/agent-loop.test.ts` event case | `ported` | R-AGENT-001 |
| `B-AGENT-002` | assistant tool-use commit 后执行一个 tool，ToolResult commit 后只发一次下一 provider turn | `packages/coding-agent/test/suite/agent-session-prompt.test.ts` 的 `handles a tool call turn and waits for the follow-up LLM response`；1717/2113 settlement file | `ported` | R-AGENT-001 |
| `B-AGENT-003` | provider error/abort 产生唯一 terminal assistant；preflight rejection 不启动 run | Agent lifecycle failure tests；AgentSession no-model/no-auth tests | `ported` | R-AGENT-001 |
| `B-AGENT-004` | missing/invalid/failing tool 形成 error ToolResult；late update 丢弃 | Agent tool failure 与 late-update tests | `ported` | R-AGENT-001 |
| `B-AGENT-005` | single active run、busy、幂等 abort、waitForIdle 包含 durable/listener settlement | `packages/agent/test/agent.test.ts` busy/async subscriber cases；`packages/coding-agent/test/suite/regressions/6363-agent-settled-event.test.ts` | `ported` | R-AGENT-001 |
| `B-AGENT-006` | AgentHarness 独有 embedding/save-point/shutdown/async-store 能力 | `packages/agent/src/harness/` 与 harness tests | `deferred` | standalone core 稳定且出现真实非 CLI caller |
| `B-AGENT-007` | 复制 Agent、AgentSession、AgentHarness 三套 class/runtime 结构 | [AGENT_PATHS.md](AGENT_PATHS.md) | `not-applicable` | 只提取 behavior/invariant，永不按 class 完成度迁移 |
| `B-AGENT-008` | tool 中途 cancel 后 commit error ToolResult 和 synthesized aborted assistant，不发第二次 provider call | `packages/agent/src/agent-loop.ts::executeToolCalls*`；pi-go strengthened lifecycle matrix | `intentionally-incompatible` | R-AGENT-001；上游 post-cancel provider call 明确 incompatible |
| `B-AGENT-009` | `length + toolCall` 截断不执行，并形成关联 error ToolResult 后继续 | `packages/agent/test/agent-loop.test.ts` length tool-call case | `deferred` | M-BASE 支持 mixed length/tool terminal 后重评 |
| `B-AGENT-010` | 同一 assistant turn 的 multiple tool call、global parallel/sequential 与 tool-level sequential override；durable result source order | `packages/agent/src/agent-loop.ts::executeToolCalls*`；`types.ts::ToolExecutionMode` | `ported` | R-AGENT-002 |
| `B-AGENT-011` | parallel completion event 与 source-order artifact 分离；missing/failure/terminate/cancel batch 和 settled late update isolation | `agent-loop.ts::executeToolCallsParallel/prepare/finalize`；agent loop tests | `ported` | R-AGENT-002 |
| `B-AGENT-012` | steering/follow-up FIFO queue drain mode、snapshot/clear、Continue assistant-tail admission 与 durable queue ack | `agent.ts::PendingMessageQueue/steer/followUp/continue`；agent tests | `ported` | R-AGENT-002 |
| `B-AGENT-013` | provider 前 immutable context transform；error/cancel 不静默 fallback | `agent-loop.ts::streamAssistantResponse`；agent-loop transform tests | `ported` | R-AGENT-002 |
| `B-AGENT-014` | multi-worker Abort/settlement、single active run、unique terminal/usage commit | `agent.ts::runWithLifecycle`；AgentSession settlement regressions | `ported` | R-AGENT-002 |

## M-SESSION

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-SESSION-001` | atomic create v3 header，返回成功即 durable | coding `SessionManager.newSession` + `_persist/_rewriteFile`；Harness `JsonlSessionStore.create`；atomic sync 是 Go strengthening | `ported` | R-SESSION-002 |
| `B-SESSION-002` | storage-first append，唯一 ID/parent chain；失败不推进 leaf | Harness JSONL append 与 session aggregate tests | `ported` | R-SESSION-002 |
| `B-SESSION-003` | 关闭旧对象后按 path resume，重建 WF-001 四消息 context | `packages/agent/test/harness/session-backends.test.ts` 的 `writes headers and entries and reopens the aggregate`；coding session format | `ported` | R-SESSION-002 |
| `B-SESSION-004` | pre-write failure 不改文件；write/sync failure 不推进 leaf、poison writer，并显式报告磁盘结果不确定 | 项目 data safety constraint；上游缺直接 fault test | `ported` | R-SESSION-002 |
| `B-SESSION-005` | unknown header/entry/message/content round-trip，未知语义不进入 provider context | coding append-only behavior；Harness base-envelope parser | `ported` | R-SESSION-002 |
| `B-SESSION-006` | future version、middle malformed、trailing partial、duplicate/broken parent 拒绝并禁止 append | coding permissive tests；Harness strict tests | `ported` | R-SESSION-002 |
| `B-SESSION-007` | 同 session concurrent append 串行成一条 parent chain | `packages/agent/test/harness/session-backends.test.ts` 的 `serializes concurrent appends into one parent chain` | `ported` | R-SESSION-002 |
| `B-SESSION-008` | coding-agent v1/v2 自动迁移到 v3 | coding migration tests/fixtures | `implemented-awaiting-review` | Open source-snapshot/pure migration/atomic rewrite；explicit trailing-partial recovery 与 Windows cross-process lock debt 见 SESSION_STORAGE |
| `B-SESSION-009` | Harness `leaf`/retained-tail 与 coding JSONL reconciliation | Harness/coding session docs | `deferred` | branch/compaction 模块取证；不能假定互通 |
| `B-SESSION-010` | 显式 leaf select/reset；append 从选中 leaf 加 child，reset 产生合法新 root；reopen 选择 physical tail | `session-manager.ts::branch/resetLeaf/_appendEntry/_buildIndex`；tree traversal tests | `ported` | R-SESSION-003 |
| `B-SESSION-011` | tree、path、context 仅沿 selected leaf，siblings 不进入 context；malformed parent graph 仍拒绝 | `session-manager.ts::getBranch/getTree/buildSessionContext`；tree/build-context tests | `ported` | R-SESSION-003；summary/compaction projection deferred |
| `B-SESSION-012` | selected path extract 与 full forest fork 创建新 header/parentSession，source 不覆写，target publication/cancel 可诊断；commit-unknown source fail-closed | `session-manager.ts::createBranchedSession/forkFrom`；tree/custom-id tests；Go atomic strengthening | `ported` | R-SESSION-003 |
| `B-SESSION-013` | manual compaction captures selected-branch snapshot, calls injected summarizer unlocked, then atomically appends v3 compaction only when leaf/generation still match | coding `agent-session.ts::compact`；Harness `agent-harness.ts::compact`；both compaction modules | `ported` | R-SESSION-004；M-AGENT/M-APP must inject production summarizer without bypassing Session |
| `B-SESSION-014` | latest selected-path compaction projects checkpoint summary + retained tail; old prefix/siblings never enter provider context | `session-manager.ts::buildContextEntries/buildSessionContext`; build-context tests | `ported` | R-SESSION-004 |
| `B-SESSION-015` | context estimate and tool-result-safe cut point support manual preparation；all token additions fail explicitly on overflow；threshold predicate is available but not automatically invoked | coding/Harness `compaction.ts::{estimateContextTokens,findCutPoint,shouldCompact}` | `ported` | R-SESSION-004；automatic trigger/retry policy is M-AGENT/M-APP |
| `B-SESSION-016` | compaction v3 envelope validates parent, ancestor `firstKeptEntryId`, timestamp, token/cost usage and survives reopen/fork/extract/raw unknown handling | `session-manager.ts::CompactionEntry`; compaction serialization tests | `ported` | R-SESSION-004 |
| `B-SESSION-017` | stale/cancel/provider-failed compaction cannot append; post-write uncertainty poisons the aggregate | Harness operation settlement + project durable-write strengthening | `ported` | R-SESSION-004 |
| `B-SESSION-018` | branch-summary generation/cache/invalidation is intentionally not exposed; unknown branch_summary records remain durable but non-projecting | both `branch-summarization.ts`; tree navigation tests | `deferred` | M-SESSION/v0.4 requires navigation/cache contract |

## M-TOOL

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-TOOL-001` | 固定 WorkingDir 执行 Bash，合并输出并返回 execution outcome | `packages/coding-agent/test/tools.test.ts` 的 `should execute simple commands` | `ported` | R-TOOL-002 |
| `B-TOOL-002` | non-zero、missing cwd、spawn failure 形成不同 typed outcome | `packages/coding-agent/test/tools.test.ts` 的 command error/cwd/spawn cases | `ported` | R-TOOL-002 |
| `B-TOOL-003` | timeout/cancel kill process tree、wait/reap，settle 后无 late update | `packages/coding-agent/test/tools.test.ts` timeout cases；`packages/coding-agent/test/suite/regressions/5208-late-bash-output.test.ts` | `ported` | R-TOOL-002 |
| `B-TOOL-004` | UTF-8-safe tail 保留 2,000 行/50 KiB，完整输出 artifact mode 0600 | tools UTF-8/truncation/full-output tests | `ported` | R-TOOL-002 |
| `B-TOOL-005` | shell exit 后 active descendant 输出重置 100ms idle grace；quiet pipe 释放且 background child 可继续存活 | `packages/coding-agent/test/suite/regressions/5303-bash-output-truncation.test.ts` 的两个完整 case | `ported` | R-TOOL-002 |

WorkingDir 不是 sandbox root。上游允许 command 使用当前 OS account 的全部权限；任何
未来 confinement 都必须作为新的安全 behavior 明确设计，不能在 v0.1 名称中暗示存在。

### M-TOOL/v0.2 filesystem suite（ported）

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-TOOL-006` | read shared cwd path、line range、UTF-8-safe head truncation；binary 不 silent decode | `core/tools/read.ts`、`path-utils.ts`、`truncate.ts`；`tools.test.ts` read suite | `ported` | R-TOOL-005；image/NFKC gap 见 TOOL_SYSTEM debt |
| `B-TOOL-007` | follow-symlink atomic replace、mode + effective writability、same target/alias serialize、cancelled queue node 同步等待并转发 predecessor barrier | `write.ts`、`edit.ts`、`file-mutation-queue.ts` 与 queue tests | `ported` | R-TOOL-005；跨进程 locking 另建 slice |
| `B-TOOL-008` | original-snapshot unique/non-overlap edit，BOM/CRLF preservation 与可应用 multi-hunk patch | `edit.ts`、`edit-diff.ts`；tools CRLF/multi-edit tests | `ported` | R-TOOL-005；full NFKC fuzzy mapping deferred |
| `B-TOOL-009` | stable ls/find/grep、compiled scoped ignore、grep byte-only truncation、entry/discovery/walk cancellation | `ls.ts`、`find.ts`、`grep.ts`；tools search suites | `ported` | R-TOOL-005；Rust regex/fd/gitignore parity debt |
| `B-TOOL-010` | registry dispatch and agent named executor preserve normal unknown-tool result | tools creation/Agent tool loop evidence | `ported` | R-TOOL-005；provider tool-schema advertisement remains separate slice |

## M-APP

| ID | 可观察行为 | 上游证据 | 状态 | 依赖或重评条件 |
| --- | --- | --- | --- | --- |
| `B-APP-001` | 显式 `-p` 成功只输出 final assistant text，stderr 空，exit 0 | `print-mode.ts`；print text-mode test | `ported` | R-APP-001 |
| `B-APP-002` | terminal provider error/abort 写 stderr、exit 1、完整 teardown | `packages/coding-agent/test/print-mode.test.ts` 的 `emits session_shutdown and returns non-zero on assistant error` | `ported` | R-APP-001 |
| `B-APP-003` | SIGINT/SIGTERM/SIGHUP cancel、reap、flush，退出后无 late output/commit | print signal source；上游缺完整 SIGINT/process test | `ported` | R-APP-001；signal lifecycle 为 Go strengthening |
| `B-APP-004` | 新 application 实例按显式 path resume WF-001 session | session invalid-file test + WF-001 | `ported` | R-APP-001 |
| `B-APP-005` | production entry 装配标准 OpenAI Responses，text print 全程只运行 Go | `main.ts` runtime assembly；OpenAI Responses basic text/stream intent | `ported` | R-APP-002 |
| `B-APP-006` | `--api-key` > stored OpenAI credential > configured models key > `OPENAI_API_KEY`；选中来源失败不 ambient fallback，secret 不进诊断 | `runtime-credentials.ts`；`auth/resolve.ts`；`provider-composer.ts` | `ported` | R-APP-002；login/command value 后续重评 |
| `B-APP-007` | `--provider openai --model <id>`、`--model openai/<id>` 与默认 OpenAI model；未知 route 在副作用前失败 | `cli/args.ts`；`model-resolver.ts::resolveCliModel` | `ported` | R-APP-002；完整 catalog/fuzzy/cycling 后续重评 |
| `B-APP-008` | 无 `--session` 时在 agent dir/sessions 下按 cwd 隔离创建 durable session，显式 path 优先 | `config.ts::getAgentDir`；`session-manager.ts::getDefaultSessionDirPath/newSession` | `ported` | R-APP-002 |

## M-AUTH

| ID | 行为 | 上游证据 | 状态 | 备注 |
| --- | --- | --- | --- | --- |
| `B-AUTH-001` | auth.json API-key read/set/delete，unknown provider 保留，malformed 不覆盖 | `auth-storage.ts`；`auth-storage.test.ts` | `ported` | strict duplicate/UTF-8/root admission strengthened；R-AUTH-001 |
| `B-AUTH-002` | private admission 与 atomic/durable replacement | `auth-storage.ts`；`auth-storage.test.ts` | `ported` | Unix 0600；Windows persistent auth fail-closed；R-AUTH-001 |
| `B-AUTH-003` | context-aware same-process 与 cross-process serialization | `auth-storage.ts`；concurrent modification tests | `ported` | same/different Store、取消、release/merge、re-exec、race；R-AUTH-001 |
| `B-AUTH-004` | runtime override 及 stored/configured/environment source ownership | `runtime-credentials.ts`；`runtime-credentials.test.ts`；`auth/resolve.ts` | `ported` | production 使用同一 resolver，request key 不持久化；R-AUTH-001 |
| `B-AUTH-005` | literal/environment template 与 command safe refusal | `resolve-config-value.ts`；`resolve-config-value.test.ts` | `ported` | command process 不启动，待安全/process slice 重评；R-AUTH-001 |

## 首个 workflow 之外的明确分类

- Prompt cache、multiple models、thinking/image、parallel tools、steering/follow-up、retry、
  compaction、branch/tree、settings/auth、dynamic catalog、JSON/RPC/interactive/TUI 均为
  `deferred`，按新的 behavior ID 和真实依赖逐项进入。
- Legacy global provider registration/unregistration、TypeScript conditional generic、TypeBox、
  EventStream class、Node dynamic import 和 package/class 层次属于实现细节，相关 test
  可归为 `not-applicable`，不能据此删除其承载的产品行为。
- Extension 与 remote module 按 ROADMAP 后续阶段处理，当前不创建 placeholder package、
  protocol 或 public API。
