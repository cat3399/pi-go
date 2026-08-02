# M-BASE：基础语义 charter

状态：`in-progress`（`M-BASE/v0.2-rich-content-replay`；v0.1 已复审）

首个里程碑：`M-BASE/v0.1-text-tool-stream`

## 负责

- `user`、`assistant`、`toolResult` 三类 LLM conversation message；
- text、image、thinking 和 tool call content block 的最小稳定语义；
- tool call/result 关联、usage、finish reason 与 assistant terminal outcome；
- provider stream event 的词汇、顺序约束和唯一终态；
- 后续请求确实需要 replay 的有限 provider metadata。

## 明确不负责

- Provider SDK payload、HTTP header、认证、retry 或 registry；
- tool schema 选择、参数验证和 tool 执行；
- coding-agent 的 bash/custom/branch/compaction/UI message；
- session JSONL/SQLite record 或任何外部 wire format；
- 把所有 vendor option、diagnostic 或 arbitrary metadata 变成共享字段。

## 上游证据

主实现证据：

- `packages/ai/src/types.ts` 的 message、content、usage、stop reason、model 和
  `AssistantMessageEvent`；
- `packages/ai/src/utils/event-stream.ts` 的 terminal/result contract；
- `packages/ai/src/providers/faux.ts::streamWithDeltas` 的 block/event 顺序；
- `packages/agent/src/types.ts` 的 tool result 和 stream contract；
- `packages/agent/src/agent-loop.ts` 的 tool call/result 关联与 length-truncated rule；
- `packages/ai/src/api/transform-messages.ts` 的 cross-model metadata replay 规则。

关键测试意图由 [../TESTS.md](../TESTS.md) 追踪，首里程碑主要来自：

- `packages/ai/test/faux-provider.test.ts`；
- `packages/agent/test/agent-loop.test.ts`；
- `packages/ai/test/total-tokens.test.ts`；
- `packages/ai/test/openai-responses-terminal-event.test.ts`；
- `packages/coding-agent/test/suite/agent-session-prompt.test.ts`。

## Contract 与 invariant

- 内部统一把 user text 表达为 content block 列表；上游 `string | blocks` 只在兼容
  adapter 输入处归一，不能形成两套长期内部表示。
- Assistant content 保持 block 源顺序；stream `contentIndex` 在一条 message 内稳定。
- Tool call 至少有非空 `id`、`name` 和合法 JSON object arguments；tool result 使用同一
  call ID，并明确 `isError`。参数的 typed validation 属于 M-TOOL 边界。
- v0.1 token usage 只接受非负整数 component；Go domain 使用无符号整数，provider
  adapter 必须在转换前拒绝负数、非整数、NaN、Inf 和越界 raw number。`totalTokens`
  不是可独立传入的字段，而是 input、output、cache read/write 的 checked sum；溢出
  返回错误。Reasoning 如存在必须不大于 output，`cacheWrite1h` 如存在必须不大于
  cacheWrite。
- 上游 `Usage.cost` 的单位、精度、舍入和非法值没有由首个 workflow 给出裁判，且 fake/
  agent 不消费价格。它不进入 v0.1 domain usage；在真实 provider pricing/catalog
  slice 以 `B-BASE-006` 重评，不能先用 `float64` 和猜测的校验规则固化。
- `pending` 只属于 partial assistant；成功终态为 `stop`、`length`、`toolUse`，失败
  终态为 `error` 或 `aborted`。
- Stream 正常顺序是 start、每个 block 的 start/delta/end、一个 done；失败以一个
  error 终止。EOF、重复 terminal、pending final 或非法 block 顺序都是 protocol error，
  不得当作成功。
- Terminal event 携带的 message 与 stream result 是同一语义结果。Provider runtime
  failure 不通过一个旁路 exception 避开 terminal contract。
- Failure 同时保留受控展示文本和可选 typed cause；provider 自己拥有 category/status/
  vendor code，M-BASE 只负责让同一 cause 从 event 贯穿 snapshot 与 terminal result。
- Stream event 中的 partial/message 是不可回写 snapshot；生产者后续 mutation 不得
  改变已经交付的事件，避免复制上游浅拷贝造成的数据竞争。
- Domain message 不直接作为 durable record marshal；M-SESSION 负责显式转换和未知
  数据保留。

## v0.1 assistant validity matrix

下表是 pi-go constructor 的可执行裁判，不声称上游宽 TypeScript interface 已阻止这些
组合。标为“加强”的规则用于让非法终态在 provider adapter 边界显式失败。

| 语义 | stop reason | content | error message | Owner slice |
| --- | --- | --- | --- | --- |
| partial stream snapshot | 只能 `pending` | 已完成的 text prefix；后续 block slice 再扩展 | 不存在 | B-BASE-003 |
| successful text terminal | `stop` 或 `length` | 零个或多个 text block，保持原顺序 | 不存在 | B-BASE-001 |
| tool-use terminal | 只能 `toolUse` | 至少一个完整 tool call，可混合先行 text | 不存在 | B-BASE-002 |
| failed terminal | `error` 或 `aborted` | 可保留已完成的诊断 text，但不暴露可执行 tool call | 非空受控文本 | B-BASE-004 |

`B-BASE-001` 的 constructor 只构造表中的 successful text terminal，所以不能接收
`pending`、`toolUse`、`error`、`aborted` 或 `errorMessage`。空响应不在没有上游证据时
被擅自判错；是否允许它成为 CLI 的成功输出由 M-APP 决定。User/assistant text 必须是
合法 UTF-8，避免 Go arbitrary byte string 在 JSON/stream 边界被静默替换；空字符串
保真允许。这是相对上游 JavaScript string 的显式 Go 加强。

`length` terminal 即使 provider 传来 tool-like partial data也不得成为可执行 tool call；
该规则由 `packages/agent/test/agent-loop.test.ts` 的完整 test
`should not execute tool calls from a length-truncated assistant message` 支持。Failure
terminal 的 partial 保留与 error text 细节在 B-BASE-004 开始前再由其测试矩阵闭合。

## Provider metadata

后续 adapter 已有证据需要保留：`responseId`、实际 routed `responseModel`、规范化前的
`rawStopReason`，以及同 provider/API/model replay 所需的 text/thinking/tool signature。
这些字段按 adapter slice 显式加入，不能先创建一个无限制 metadata map。

跨模型 replay 的证据规则是：失效的 opaque/redacted thinking 不发送；可读 thinking
降级为 text；tool `thoughtSignature` 删除；signed empty block 只在同模型保留。SDK
response、任意 header/body、stack 和未经筛选的 diagnostic 不进入 domain 或 session。

## Go 重新决策

- TypeScript tagged union 改为按上表分开的 Go value/constructor；constructor 只阻止
  本 slice 已定义的非法组合，不把尚未取证的跨字段 policy 冒充上游行为。具体内部
  类型仍可在多个真实消费者接入前调整，不公开。
- `ToolCall.arguments: Record<string, any>` 改为保真 raw JSON 与 tool 边界 typed decode
  分层，不让 untyped map 在 runtime 中扩散。
- Domain timestamp 使用 Go 时间值，provider/session adapter 明确转换 Unix 毫秒；
  clock 可注入测试。
- 不迁移 TypeBox、conditional generic、declaration merging 或 EventStream class。
- Stream lifecycle 不能出现“events 已关闭但 result 永远不完成”的状态；终态是强制
  contract，而不是调用者约定。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-BASE-001` | canonical user text、successful text terminal 与整数 token usage 裁判 | WF-001 | `ported` |
| `B-BASE-002` | tool call 与 tool result 的 ID、arguments、content 和 error 关联 | WF-001 | `ported` |
| `B-BASE-003` | start/text events/done 的严格顺序、snapshot 与唯一 result | WF-001 | `ported` |
| `B-BASE-004` | error/aborted、unexpected EOF、pending final 和 duplicate terminal 的失败语义 | WF-001 | `ported` |
| `B-BASE-005` | immutable thinking/image、mixed assistant content 与受控 Responses replay metadata | production Responses replay | `in-progress` |
| `B-BASE-006` | provider cost 的单位、精度、舍入、total 与非法 raw number policy | 真实 provider pricing | `deferred` |

`B-BASE-001` 是第一个实现 slice。随后在同一 module 内按
`B-BASE-002 -> B-BASE-003/004` 完成 v0.1，再通过独立 review；M-PROVIDER 和 M-TOOL
只能在该 review 后依赖这些 contract。`B-BASE-005/006` 分别等待 image/thinking 与真实
pricing 消费者，避免为尚未出现的调用者提前扩张类型。

## v0.1 退出与 review gate

- `B-BASE-001` 至 `B-BASE-004` 的正常和非法输入均有 table-driven Go tests；stream
  contract 有 malformed-order/duplicate-terminal/EOF tests，必要 seed 进入 fuzz test；
- Package 不依赖 network、Node.js、真实 credential、provider SDK 或 session format；
- API 仍位于 `internal`，没有 arbitrary common/metadata 容器；
- [../REVIEWS.md](../REVIEWS.md) 中 M-BASE 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。

## v0.2 rich-content/replay scope

`B-BASE-005` 增加 immutable `ThinkingBlock`、data/HTTP(S) `ImageBlock`、mixed assistant
content 及 image-capable user/tool-result message。image media type、source、UTF-8、base64/data
size和 defensive copy 在 admission 处裁判。Responses 仅保存可重放的 reasoning item ID/
encrypted content、text message ID/phase、response ID/raw stop reason；不保存 SDK object、任意
raw map、header/body 或 diagnostic。error/aborted assistant 永不 replay；readable unsigned
thinking 仅可降级为 text。session v3 同时保留这些 typed fields 和 unknown 原始 JSON。

完成实现不等同独立 review：本里程碑仍须由未参与者审查，不能覆盖 v0.1 的通过结论。
