# M-AGENT：Agent runtime charter

状态：`implemented, pending independent review`（`M-AGENT/v0.3-context-retry-lifecycle`；v0.1/v0.2 均已复审）

首个里程碑：`M-AGENT/v0.1-single-tool-loop`

最近完成里程碑：`M-AGENT/v0.2-multi-tool-queues`

## v0.3 context-retry-lifecycle

### 负责

- 每次 provider attempt 从 `Session.BuildContext` 的不可变 selected-leaf snapshot 取 context；
  transform 仍只是 request-local projection，绝不回写 transcript；
- 在 prompt/turn boundary、provider request 之前，以 `contextWindow - reserve` 判定自动压缩，
  并调用既有 `Session.Compact` 的 snapshot/commit gate；`Agent.Compact` 提供同一 real
  summarizer seam 的 idle/manual 入口；
- active run 是 compaction、retry wait、provider、queue 与 terminal 的唯一 coordinator；所有
  provider/storage/summarizer/sleeper 调用均在 coordinator mutex 外，`WaitForIdle` 要等到它们
  及 observer settle；
- 仅对 transport 与 408/409/425/429/5xx provider failures 做 bounded exponential retry；server
  `Retry-After` 在 provider adapter 归一，并由 policy cap。cancel、auth/config/invalid request、
  invalid response、tool 与 storage failure 不重试。

### 关键不变量与取舍

- `RetryPolicy.MaxAttempts` 包含首个请求。未 accepted 的 transient attempt 从不 append user、
  assistant 或 tool durable record；只有最终 accepted terminal 进入 usage/transcript，因此 totals
  不会因重试重复计数。stream 无 terminal 的 transport drop 也遵循这条规则。
- 自动 compaction 每个 logical provider turn 最多一次；retry 只重新取得 immutable context
  snapshot，不会在刚写 checkpoint 后再次 Compact。conflict/cancel/summary error/commit-unknown 均
  fail-explicit，后者沿 Session poison quarantine 传播，不能重试或伪装为 provider failure。
- steering/follow-up 可以在 compact/retry 期间入队，但只在既有 provider/tool boundary reserve；
  因而它们不会进入已经发出的 summary/request，也不会跨 retry 造成重复 durable user entry。
- 默认 retry 为一次请求（关闭重试）。jitter/sleep 是可注入 seam；sleep 尊重 active cancel，
  remote Retry-After 不得形成无上限等待。没有把 retry 放进 OpenAI adapter，避免 adapter 持有
  Agent queue、Session 或 run ownership。

### 验收与延期

- Go integration fixture 驱动 real OpenAI Responses HTTP/SSE adapter：context 超阈值先 durable
  summary，chunked transient stream drop 后 retry，最终 text success，断言 session 无重复 prompt/
  assistant 且有一个 compaction checkpoint；另有 unit/race/fault gates。
- 真实 credential smoke 仍显式延期；production CLI 尚未暴露 context window/retry/manual compact
  flags，后续 M-APP 只负责配置/用户 surface，不得绕过本 coordinator 和 `Session.Compact`。
- 不实现 TUI、tool wire replay、mixed length/tool terminal 或 Harness 独立 runtime；这些保持原有
  deferred behavior。此实现尚未独立 review，不得把 v0.1/v0.2 review 结论延伸到本 milestone。

## v0.2 multi-tool queues

### 负责

- 同一 assistant message 的完整 multi-tool batch，默认并行、全局 sequential 与 tool-level sequential override；
- 并发 worker 的 completion-order lifecycle event，及独立、固定的 assistant source-order durable ToolResult commit；
- missing/failure/cancel/termination 混合 batch、settled update isolation，以及 batch 后的 provider barrier；
- steering/follow-up FIFO queue、`one-at-a-time`/`all` drain、snapshot/clear、idle/busy/Continue admission；
- 每个 provider request 前的 immutable `TransformContext` seam，及其 cancel/error 的 fail-explicit contract。

### 不负责

- M-BASE 尚未表达的 `length + toolCall` 混合 terminal、tool schema wire、before/after extension hooks、retry、compaction 或 Harness storage；
- filesystem tool 的实现；v0.2 只消费 named execution port。M-TOOL/v0.2 已由主线
  `R-TOOL-005` 独立复审，不与 Agent review 合并计数。

### v0.2 contract

- 先 durable commit assistant tool-use，才开始任一副作用 worker；所有 worker settle 后，ToolResult 严格按 assistant block 的 source order append，下一 provider request 因此永远读取可解释顺序。
- 并行 `tool_settled` 是 completion order；它绝不决定 transcript order。任一个 requested tool 声明 sequential 时，整批 downgrade 为 sequential，避免隐蔽依赖竞争。
- worker 和 report closure 都不拥有 transcript。`Execute` 返回后的 update 被 gate 丢弃；Abort 取消 worker context 并等待全部 worker 结算，之后由 coordinator durable 记录各 call 的已知 outcome 和唯一 aborted assistant。
- steering 只在一个 assistant/tool batch 完整结束后、下一 provider request 前 reserve；follow-up 只在本来会停止时 reserve。两队列均 FIFO，默认 `one-at-a-time`，可显式 `all`；每条 user Append durable 后才逐条 ack/remove，失败项与后继保持原 FIFO，clear 只影响未 reserved 项。
- `Continue` 只接受 durable user/tool-result tail；assistant tail 必须先由 queued steering/follow-up 形成新的 user tail。admission 先在 mutex 内 reserve single-run slot，锁外读取 transcript，再在 mutex 内验证 tail/queue 并安装 active run；busy admission、Abort 与 WaitForIdle 仍由同一个 active run slot 管辖。
- Transform 输入和输出都复制；它仅改变本次 provider request，绝不修改 session transcript。transform error 是 `ErrContextTransform` 并在 provider 调用前 fail；cancel 在 seam 后形成 aborted terminal。

### v0.2 behavior slice

| ID | 行为 | Workflow | 状态 |
| --- | --- | --- | --- |
| `B-AGENT-010` | multiple tool calls、global/tool override 与 source-order durable result | WF-003 | `ported` |
| `B-AGENT-011` | parallel completion event、mixed missing/failure/terminate 与 late-update isolation | WF-003 | `ported` |
| `B-AGENT-012` | steering/follow-up queues、snapshot/clear、Continue and busy/idle | WF-003 | `ported` |
| `B-AGENT-013` | immutable transform before every provider call; error/cancel contract | WF-003 | `ported` |
| `B-AGENT-014` | multi-worker Abort, settlement barrier, unique usage/terminal | WF-003 | `ported` |

### v0.2 review gate

- `R-AGENT-002` 最终结论为 `passed`，0 Blocker / 0 Major / 0 Minor；实现与修订候选为
  `80d4094`、`84a8c93`、`7e587b9`、`7cbc1c5`。
- 定点 normal/error/cancel/fault/concurrency tests、`go test -race ./...`、llm/session fuzz、
  Linux/Windows amd64 与 Darwin arm64 compile 及累计 diff check 全部通过。
- M-TOOL filesystem 的最终独立结论属于主线 `R-TOOL-005`；本里程碑只核验 named-tool
  consumer，不替代或重复该 review。

## 负责

- Accepted run 与 turn lifecycle；
- provider event 到 assistant partial/final outcome 的归一；
- 一个 sequential tool call、tool result 和下一次 provider turn 的因果控制；
- terminal success/error/abort 的唯一提交；
- single-active-run、取消和 settlement barrier；
- 对下层 provider、tool、session port 的顺序编排。

## 明确不负责

- CLI/TUI 输出、terminal state、auth 与 model discovery；
- 具体内置 tool 的 filesystem/subprocess 实现；
- JSONL/SQLite record schema 与文件恢复；
- 首里程碑之外的 parallel tool、steering、follow-up、retry、compaction、branch、
  extension hook 或 public API；
- 复制 `AgentSession`、`Agent` 或 `AgentHarness` class hierarchy。

## 上游证据

主实现证据：

- `packages/agent/src/types.ts` 的 `StreamFn`、`AgentEvent`、`AgentState`、
  `AgentLoopConfig`；
- `packages/agent/src/agent.ts` 的 `Agent.prompt`、`runWithLifecycle`、`processEvents`、
  `handleRunFailure`、`waitForIdle`；
- `packages/agent/src/agent-loop.ts` 的 `runAgentLoop`、`runLoop`、
  `streamAssistantResponse`、`executeToolCalls*`、`prepareToolCall`；
- `packages/coding-agent/src/core/agent-session.ts` 的 prompt、事件持久化和 settlement
  路径；
- [../AGENT_PATHS.md](../AGENT_PATHS.md) 的调用链与 Harness 分类。

关键 test intent 由 [../TESTS.md](../TESTS.md) 追踪，主要来源是：

- `packages/coding-agent/test/suite/agent-session-prompt.test.ts`；
- `packages/agent/test/agent-loop.test.ts`；
- `packages/agent/test/agent.test.ts`；
- `packages/coding-agent/test/suite/regressions/1717-2113-agent-session-event-settlement.test.ts`；
- `packages/coding-agent/test/suite/regressions/6363-agent-settled-event.test.ts`。

## Contract 与 invariant

- Accepted run 的状态机初始为
  `Idle -> ProviderTurn1 -> Tool1 -> ProviderTurn2 -> Settling -> Idle`。
- 每个 session 只有一个 coordinator 可以修改 phase、active run identity、turn index、
  transcript projection、queue 和 pending tool identity。
- Provider/tool worker 不直接写 session；所有结果携带 run/turn/call identity，由 owner
  拒绝迟到或属于旧 session generation 的提交。
- 每个 turn 使用不可变 model/tool/system/config snapshot。
- Assistant tool-use 的最终版本先成功 commit，再启动副作用 tool；tool result 先成功
  commit，再请求下一 provider turn。
- Terminal outcome 对每个 accepted run 恰好提交一次。Idle 只在 producer、tool、
  durable append 和 awaited observer 全部 settle 后可见。
- Provider terminal error/abort 是 run outcome；tool missing/invalid/failure 是关联原
  call 的 error result；storage 或 invariant failure 是 fatal，不能伪装成 provider 或
  tool success。
- Cancellation scope 贯穿 provider 与 tool。Abort 幂等，并等待所有仍可能提交状态的
  goroutine 退出；`context.Context` 不保存为跨 run 配置。
- Tool 执行中 cancel 的 owner 是 coordinator，不是 M-TOOL。若 cancel 在正常 tool
  outcome 被 coordinator 接受前线性化，coordinator 先等待 tool/process settle，再用独立
  的 bounded settlement context durable commit：
  `ToolResult{isError:true, text:"Tool execution cancelled"}`，
  然后 commit `Assistant{stopReason:aborted, content:[], errorMessage:
  "Run cancelled during tool execution", usage:zero}`。两条都关联当前 run/call，之后直接
  Settling，不发起 ProviderTurn2；该场景 provider call count 恰为 1。
- Settlement deadline 只允许在 session 首次 write 前取消；write 开始后必须同步等待
  commit 成功或 outcome unknown，即使超过 deadline。不得用 goroutine 包装 Append，
  否则 `Run()` 返回后仍可能发生 late commit。
- 若正常 ToolResult 已 durable commit 后才观察到 cancel，则保留该 result，直接合成同一
  aborted assistant，不开始下一 provider turn。若 ProviderTurn2 已开始，则由 provider
  cancellation contract 产生唯一 aborted terminal。任何 storage commit failure 都升级为
  fatal storage outcome，不伪称 durable cancellation 已闭合。
- 首里程碑只有一个 tool，因此 transcript order 与 execution order 相同。未来并行
  slice 必须单独证明 completion-order event 与 source-order durable commit。

## 依赖与 ownership

- 依赖 M-BASE 的 message、content、finish、usage 和 provider event 值语义；
- 依赖 M-PROVIDER 的窄 stream port；
- 依赖 M-TOOL 的窄 execution port；
- 依赖 M-SESSION 的 context snapshot 与有序 append/commit port；
- 不依赖 M-APP、CLI、TUI、extension 或 remote transport。

Port 由 Agent runtime 的真实消费行为定义，不为上游每个 class 创建 interface。
Durable record 与 filesystem ownership 仍在 M-SESSION；Agent 只控制何时必须等待提交
barrier。

## Go 重新决策

- 不迁移 TypeScript EventStream class、全局 default stream fallback 或 declaration
  merging。
- 不暴露可写 transcript slice/map；API 返回 snapshot。
- 不持有 mutex 跨 provider、tool、storage 或 callback 等待。Coordinator 顺序提交，
  worker 只报告结果。
- 不用 goroutine-per-event、unbounded channel 或 fire-and-forget observer。Tool promise
  结束后的 update 必须被丢弃。
- Crash recovery 不自动重跑未确认完成的有副作用 tool。
- 上游 tool abort 会先形成 error ToolResult，随后可能用已取消 signal 再进入 provider；
  pi-go 选择上述 coordinator-synthesized terminal，减少一次无业务意义的 provider call。
  这是显式 `intentionally-incompatible` lifecycle decision，transcript 更闭合但不改变
  tool side effect 的不确定性。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-AGENT-001` | idle prompt 产生完整单轮 stream 与 terminal assistant lifecycle | WF-001 | `ported` |
| `B-AGENT-002` | assistant tool-use commit 后执行一个 tool，tool result commit 后继续下一 provider turn | WF-001 | `ported` |
| `B-AGENT-003` | provider error/abort 产生唯一 terminal outcome；preflight failure 不启动 run | WF-001 | `ported` |
| `B-AGENT-004` | tool missing、invalid 或 execute failure 归一 error result；late update 被丢弃 | WF-001 | `ported` |
| `B-AGENT-005` | busy、幂等 abort 与 wait-for-settlement 保持 single-active-run invariant | WF-001 | `ported` |
| `B-AGENT-008` | tool 中途 cancel durable commit error ToolResult + synthesized aborted assistant，且不再调用 provider | WF-001 | `intentionally-incompatible` |
| `B-AGENT-009` | `length + toolCall` 截断不执行，并形成关联 error ToolResult 后继续 | 后续 | `deferred` |

`B-AGENT-001` 至 `005` 已由一个完整模块实现和联合复审关闭；`B-AGENT-008` 是已验证
的 Go 生命周期加强。`B-AGENT-009` 等待 M-BASE 能表达 mixed length/tool terminal 后
再实现，不能用 text-only length 用例代替。

v0.1 的 durable ToolResult 只承诺 call ID/name、`isError` 和 text；稳定 category/details
等待真实消费者出现并由 M-BASE/M-SESSION 共同设计。当前 provider request 也尚无 tool
schema，scripted 闭环已成立，但真实 provider 的 tool discovery 留给对应 adapter slice。

## v0.1 退出与 review gate

- 上述五个 behavior 及 `B-AGENT-008` 的正常、错误、取消与 session barrier 均有 Go contract/scenario
  test；涉及 shared state 的测试通过 `go test -race`；
- WF-001 能观察 `user, assistant(toolUse), toolResult, assistant(final)` 顺序；
- 没有第二份可独立写的 mutable transcript，没有旧 generation late commit；
- [../REVIEWS.md](../REVIEWS.md) 中 M-AGENT 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。

该里程碑通过不表示 parallel tool、queue、retry、compaction 或 Harness 独有能力已
迁移；它们继续保留独立 behavior 与重评条件。
