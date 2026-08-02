# Agent 产品路径分类

本文分类固定上游中低层 `Agent`、coding-agent `AgentSession` 与独立 `AgentHarness` 的
实际地位。结论用于防止 pi-go 因上游 class 数量而创建重复 runtime、session、tool
或 compaction 实现。

## 结论

当前 coding-agent executable 的唯一产品主线是：

~~~text
packages/coding-agent package bin
  -> src/cli.ts
  -> main.ts::main
  -> createAgentSessionServices / createAgentSessionFromServices
  -> sdk.ts::createAgentSession
       -> new Agent
       -> new AgentSession
  -> AgentSessionRuntime
  -> print / interactive / RPC mode
  -> AgentSession.prompt
  -> Agent.prompt
  -> agent-loop.ts::runAgentLoop
  -> provider stream -> tool -> next provider turn
~~~

`packages/coding-agent/src` 不实例化或引用 `AgentHarness`。固定基线中
`packages/agent/docs/agent-harness.md` 对 coding-agent 接入仍标记为 Planned。因此
`AgentHarness` 是上游独立导出的 library product，不是当前 standalone CLI 的替代
实现，也不能被当成 pi-go 首个 workflow 的结构模板。

## 三条路径的职责

| 路径 | 当前地位 | 可提取的产品行为 | 不复制的组织 |
| --- | --- | --- | --- |
| coding-agent `AgentSession` | CLI、print、RPC 和 TUI 的产品主路径 | prompt preflight、产品 tool/resource 组合、低层事件与 session 持久化 barrier、retry/compaction、session-level settlement | 3,332 行聚合 class、extension runner 引用、services/runtime/sdk 的具体层次、直接修改 Agent mutable state |
| `Agent` 与 `agent-loop` | 主路径直接依赖的低层 runtime | 单 active run、provider/tool loop、steer/follow-up queue、abort、事件顺序、tool 参数与错误归一 | TypeScript EventStream、TypeBox、declaration merging、全局 default stream fallback |
| 独立 `AgentHarness` | 无 production caller 的独立 library 能力 | phase/abort/task tracking、save-point snapshot、泛型 tool context、async session store、shutdown lifecycle | 第二套高层 runtime/session/tool/compaction class hierarchy |

## 共享 invariant

以下语义同时得到当前主路径与独立 Harness 的支持，可以作为 Go domain 证据：

- 一个 session 同一时刻最多有一个 structural agent run；普通并发 prompt 返回 busy。
- 每次 provider request 使用不可变 turn snapshot。运行中的 model、tool、system prompt
  或配置变化只能在下一安全点生效。
- 请求 tool 的 assistant message 必须先完成最终 commit，副作用 tool 才能开始。
- Tool result 必须先 commit，下一次 provider request 才能读取它。单 tool 成功闭环的
  conversation 顺序是 `user, assistant(toolUse), toolResult, assistant(final)`。
- 已接受的 run 即使以 provider error 或 abort 结束，也产生一个可观察 terminal
  assistant outcome 并完成 turn/run settlement；busy、缺 model 或缺 auth 等 preflight
  失败不启动 run。
- Tool 不存在、参数不合法、hook block、tool throw 等执行问题归一为关联原 call 的
  error tool result，通常仍允许下一轮 model 解释。`length` 截断的 tool call 不执行。
- Cancellation 传入 provider、tool 和 hook；只有 producer、内部提交、listener 和
  durable barrier 都结束后才能公开 Idle。
- 并行 tool 的 execution completion order 与 transcript source order 是两个语义；
  首个 slice 只支持一个 sequential tool，不提前引入该复杂度。

## 不能提前统一的差异

- `AgentSession` 的 extension message replacement 与持久化/公开 listener 顺序，和
  `AgentHarness` 的 append-then-notify 顺序不同。pi-go 只保留“依赖该 message 的下个
  阶段开始前，最终版本已经 commit”这一共享 invariant；observer 顺序由 Go workflow
  的真实需求决定。
- `AgentSession.abort()` 不自动清 queue；`AgentHarness.abort()` 清 steer/follow-up 但
  保留 next-turn message。Steering 尚未进入首个 slice，不能现在发明统一规则。
- Harness 的 auto retry、auto compaction 和 crash recovery 仍有 planned 能力，不能
  把设计文档当成固定基线已经实现的行为。

## pi-go 决策

- 只建立一套 agent runtime、session invariant、tool lifecycle 和 compaction 语义。
- 每个 active session 使用一个 coordinator 作为 volatile state 的唯一写者；provider
  和 tool worker 只能返回带 run/turn/call identity 的结果，不能直接写 transcript。
- Durable session 是 conversation 的 source of truth。Agent 通过窄提交能力建立
  assistant-before-tool 与 toolResult-before-next-provider barrier，不维护可独立漂移的
  第二份 mutable transcript。
- 每个 accepted run 使用自己的 cancellation scope。Abort 要等待 provider、tool、
  update channel、durable append 和 awaited observer settle，迟到 generation 不得提交。
- 首个 slice 只做单 tool sequential loop；parallel tool、steering、retry、compaction、
  extension facade 与 public API 都按后续 behavior 重新取证。
- Harness 独有的 generic embedding、save-point dynamic resource refresh、async store 和
  shutdown API 保持 `deferred`。重新评估条件是 standalone coding-agent core 已稳定，
  且出现不属于当前 CLI 主路径的真实调用者或上游同步要求。

## 直接证据

产品调用链和低层 runtime：

- `packages/coding-agent/package.json` 的 `bin.pi`；
- `packages/coding-agent/src/cli.ts` 顶层 `main` 调用；
- `packages/coding-agent/src/main.ts::main`；
- `packages/coding-agent/src/core/agent-session-services.ts` 的 session services/factory；
- `packages/coding-agent/src/core/sdk.ts::createAgentSession`；
- `packages/coding-agent/src/core/agent-session-runtime.ts::AgentSessionRuntime`；
- `packages/coding-agent/src/core/agent-session.ts` 的 `prompt`、`_runAgentPrompt`、
  `_handleAgentEvent`、`_handlePostAgentRun`、`abort` 和 `waitForIdle`；
- `packages/agent/src/agent.ts` 的 `prompt`、`runWithLifecycle`、`processEvents`；
- `packages/agent/src/agent-loop.ts` 的 `runAgentLoop`、`runLoop`、
  `streamAssistantResponse` 和 `executeToolCalls*`。

独立 Harness：

- `packages/agent/src/harness/agent-harness.ts`；
- `packages/agent/src/harness/session/`；
- `packages/agent/src/harness/types.ts`；
- `packages/agent/docs/agent-harness.md`；
- `packages/agent/docs/durable-harness.md`。

首个 workflow 的关键上游测试名称已登记在 [TESTS.md](TESTS.md)，对应 behavior 见
[BEHAVIORS.md](BEHAVIORS.md) 和 [modules/AGENT_RUNTIME.md](modules/AGENT_RUNTIME.md)。
