# M-PROVIDER：AI 与 provider runtime charter

状态：`in-progress`（`M-PROVIDER/v0.3-openai-tools-replay`；v0.1/v0.2 已复审）

最近完成里程碑：`M-PROVIDER/v0.2-openai-responses-text`

当前里程碑：`M-PROVIDER/v0.3-openai-tools-replay`

## 负责

- 根据明确的 provider/model/API dialect 选择 stream adapter；
- auth resolution、request conversion、stream parsing、error mapping 与必要 retry；
- 把 setup、auth、transport、parser 和 runtime failure 归一为 M-BASE terminal stream；
- 保留后续 turn 所需且经过验证的 vendor metadata；
- 测试使用的 deterministic scripted provider。

## 明确不负责

- Agent turn/tool loop、session durability 或 CLI 输出；
- 内置 tool 执行；
- 全局 mutable compat registry 或 default provider fallback；
- 把每个 provider registration 文件变成独立 Go architecture module；
- 首里程碑中的 OAuth、prompt cache、dynamic catalog、image generation、retry matrix 或
  cross-provider handoff。

## 上游证据

- `packages/ai/src/models.ts` 的 provider/runtime dispatch 与 auth application；
- `packages/ai/src/types.ts` 的 `StreamFunction`、model 和 stream option contract；
- `packages/ai/src/providers/faux.ts` 的 scripted queue 与 event producer；
- `packages/ai/test/faux-provider.test.ts` 的 FIFO、factory、error、order 和 cancellation；
- `packages/ai/test/providers.test.ts` 与 `packages/ai/test/models-runtime.test.ts` 的 mixed
  dialect dispatch、unknown provider/API 和 auth/error 行为；
- `packages/ai/src/api/openai-responses.ts` 的 request/client/error wrapper；
- `packages/ai/src/api/openai-responses-shared.ts` 的 SSE parser/terminal contract；
- `packages/ai/test/openai-responses-terminal-event.test.ts`、
  `packages/ai/test/fetch-option.test.ts` 与 `packages/ai/test/stream.test.ts` 中标准 OpenAI
  Responses 的 text/transport/smoke intent。

完整 provider/API/auth 行为族见 [../PROVIDERS.md](../PROVIDERS.md)，test intent 由
[../TESTS.md](../TESTS.md) 追踪。

## Contract 与 state ownership

- Provider 是显式注入的 runtime instance；不同 test/application instance 不共享
  mutable queue、cache、registry 或 call count。
- 每次 stream 调用消费一个明确 request snapshot，provider 只拥有该 request 的 event
  producer，不修改 agent transcript 或 durable session。
- Scripted provider 以 FIFO 分配 response step。Step 可以读取 request context、model、
  options 和 call index；queue exhaustion 与 factory failure 都产生唯一 terminal error。
- Fake 注入 clock、ID、chunk schedule 和 usage，记录收到的 request snapshot。默认不
  使用 `Date.now()`、random chunk 或 random ID，因此相关上游测试在 Go 中属于
  `strengthened`。
- Cancellation 使用调用方 context。Pre-start cancel 可以没有 start；mid-block cancel
  保留已交付 partial，以 aborted terminal 结束，并停止所有后续 event/state mutation。
- Runtime dispatch 同时校验 model 的 provider 与 API dialect；unknown provider、缺失
  dialect implementation、auth/setup failure 都返回 error stream，不 panic、不 fallback。
- Provider error 至少保留 typed category、cause、HTTP status 和受控 vendor code；上游
  string matching 只可作为兼容 fallback，不能成为 Go 主 taxonomy。

## Deterministic fake 的范围

v0.1 支持：

- replace/append scripted response steps、FIFO、call count 与 request capture；
- assistant text 和一个 tool call 的固定 event sequence；
- response factory、queue exhaustion、factory failure；
- pre-start 与 mid-text cancellation；
- 显式 terminal error/aborted。

Prompt-cache simulation、multiple model registry、thinking/image/multiple-tool streaming 延后。
上游 `unregisters the provider` 只验证 legacy global compat registry，标为
`not-applicable`。

## v0.2 标准 Responses text 范围

- 仅路由标准 `openai/openai-responses`，使用已解析 bearer credential、显式 base URL/
  HTTP client 和 system/developer role policy；adapter 不读取环境或 credential 文件。
- 一个完整里程碑同时实现 request/history mapping、HTTP/SSE、text/refusal、usage、terminal、
  typed failure、cancellation 与 body ownership，不按文件分别 review。
- `response.completed`/`response.incomplete` 先暂存，只有 `[DONE]` 或正常 EOF 才成功；终态后
  的事件、读错、malformed frame 或取消不能被先前终态掩盖。
- 未知 progress event 可在终态前忽略；tool、reasoning、image 和当前 M-BASE 无法持久回放的
  message phase 必须显式失败。Retry、prompt cache、真实 auth storage、catalog 和 production
  assembler 不属于此里程碑。

## Go 重新决策

- 不迁移 global `apiProviderRegistry`、`register/unregisterFauxProvider`、
  `setDefaultStreamFn` 或 lazy dynamic import；
- 不复制每 provider 一个薄 factory 的文件布局。API dialect 是 adapter 复用单位，
  provider/auth/catalog 由数据和少量 policy 装配；
- Provider port 由 Agent 的真实 stream 消费行为定义，不包含 OAuth/model discovery 等
  尚未出现的宽接口；
- 测试 fake 可以与生产 adapter 实现同一窄 port，但不能通过 production fallback
  自动启用。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-PROVIDER-001` | 固定 text response 的 FIFO script、request capture 与 deterministic stream | WF-001 | `ported` |
| `B-PROVIDER-002` | 固定 one-tool-call event stream，content index 与 JSON arguments 完整 | WF-001 | `ported` |
| `B-PROVIDER-003` | queue exhaustion、factory/explicit error 与 pre/mid-stream cancellation | WF-001 | `ported` |
| `B-PROVIDER-004` | 显式 provider/API dispatch；unknown/missing adapter 归一 error stream | 后续装配 slice | `deferred` |
| `B-PROVIDER-005` | 标准 `openai-responses` 基础 text streaming 与 terminal handling | 阶段 2 真实 dialect 验证 | `ported` |

M-BASE 的 stream/message contract 是前三项的直接依赖。真实 adapter 首选标准
`openai-responses`，先用本地 HTTP/SSE fixture 验证 text/terminal，再运行显式启用的
真实 provider test；不从体量最大的 completions compat matrix 起步。

## v0.1 退出与 review gate

- `B-PROVIDER-001` 至 `B-PROVIDER-003` 有 deterministic contract tests，取消测试检查
  producer 退出和 terminal 后无事件；适用测试通过 `go test -race`；
- Fake 无 global state、network、真实 credential、wall-clock 或随机依赖；
- Queue/factory error 不 throw/panic，不产生第二终态；
- [../REVIEWS.md](../REVIEWS.md) 中 M-PROVIDER 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。

Provider dispatch 与真实 adapter 分别形成后续独立里程碑和 review，不被 v0.1 的 fake
通过结论掩盖。

v0.2 已由 R-PROVIDER-004 通过整模块复审。本地 HTTP/SSE、race、fuzz、全仓 gate 和多平台
test compile 是本里程碑证据；真实 credential smoke、production assembler 以及 B-BASE-005
replay metadata 仍分别验收，不能用本地 fixture 冒充。

## v0.3 OpenAI tools/replay 边界

- `provider.Request` 增加 immutable neutral function definitions（name、description、strict、JSON-schema object）；OpenAI Responses request 逐项编码为 `tools`，重复/非 object/可变 caller bytes 在 admission 失败。
- replay 只发送完整 user、successful assistant text/function_call 与 durable ToolResult；failure/aborted partial assistant 绝不重放。function call 的 domain ID 是 `call_id|item_id`，wire output 只使用前一段 call ID；item ID 仅在可证明的 `fc_*` 形状时保留，否则稳定规范化。
- SSE 支持 source-order mixed text/function_call、arguments start/delta/done、JSON object finalization 和 `toolUse` terminal；unknown、duplicate、orphan、out-of-order、partial/invalid JSON 和 dirty EOF 显式失败，不能产生可执行 partial call。
- 本里程碑只覆盖 function tools。reasoning/image/custom tool、prompt cache，以及没有 M-BASE metadata storage 的 response/message ID 仍延期；不创建无界 metadata map。
- M-AGENT/v0.1 只消费一个 call，但 adapter/replay 可表达多个完整 calls；并行 dispatch 属 M-AGENT/v0.2。M-APP 的 local HTTP/SSE scenario 验证一个 bash call、durable ToolResult 和第二 request replay。

| ID | 行为 | Workflow | 状态 |
| --- | --- | --- | --- |
| `B-PROVIDER-006` | Responses function tool schema encoding、tool replay、strict SSE function-call reducer | WF-003 | `in-progress` |
