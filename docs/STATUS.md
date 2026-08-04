# 当前状态

本文是 2026-08-04 的实现快照，基于代码、测试和 production assembly 重新审查，不沿用
旧迁移计划的完成度判断。

最初差距审查的代码起点（不是当前实现版本）：

- pi-go：`3b39253e2b5547e05d206941fcc2feccc90f62ae`
- 原版 pi：`a116523434806910336b9de3e38a41aa5860030b`
- pi-web：`dfab5853b8d2f717df259e7ebc94f49a3c2e43e7`

当前实现状态以本文件所在提交及其测试结果为准，不再用上述 pi-go 起点哈希代表进度。

## 总体结论

pi-go 已有质量较好的底层 Agent 执行能力和一部分产品控制能力，不是从零开始；但当前
仍是“若干高级能力聚集在现有 Agent/AgentSession 中”，尚未形成与原版对应的完整
`Runtime → AgentSession → Agent → AgentLoop` 调用分层。

因此目前不能宣称 Agent 首期重写完成，也不应该开始以 pi-web 接入速度为目标实现 RPC。
最重要的工作是校正共享数据模型、补齐中间层所有权，并把已有能力装配成可长期驱动的
内部 Runtime。

## 已有可复用能力

| 区域 | 当前实现事实 |
| --- | --- |
| 流式执行 | text/thinking/tool stream、连续 tool loop、多工具并行或顺序调度、取消与 settlement |
| 富内容 | rich user input、image content、rich tool result/details 和队列输入已经进入核心类型与测试路径 |
| 动态运行 | 每个 provider turn 获取 snapshot；model、thinking、system prompt 和 tools 可在 tool chain 间变化 |
| 控制策略 | continue、steering/follow-up queue、retry 与 compaction 的主要机制已经存在 |
| Provider | deterministic fake、OpenAI Responses、OpenAI Chat Completions，以及 reasoning/tool replay 支持 |
| Session 存储 | JSONL v3、锁与原子追加、恢复、tree/branch/fork、compaction、legacy/unknown raw 保护 |
| 产品基础 | `AgentSession` 已组合一部分持久化、队列、重试、压缩和动态配置行为 |
| 工具与服务 | bash/read/write/edit/grep/find/ls，API key/OAuth，settings/model/resource/prompt 加载 |
| 诊断入口 | `cmd/pi-go -p` 可组装当前 production path 并执行一次 headless prompt |

这些事实说明许多机制可以迁移或重组，不说明它们已经处于正确层次，也不说明 production
assembly 已经启用全部能力。

## 与目标架构的主要差距

### P0：兼容数据模型已完成

P0 已按原版类型、序列化行为和真实调用边界完成复审与修正。这里的“完成”只表示共享
数据契约已经可以支撑后续 AgentLoop、Agent、SessionManager 和 AgentSession 工作，不表示
这些更高层阶段已经完成。

- AgentMessage 的 LLM、bash、custom（含 string-vs-blocks）、opaque extension 消息均可原样
  写入 v3 JSONL、重开和沿分支投影；`ConvertToLLM` 是唯一的 LLM 投影边界；
- selected branch 会派生 effective model/thinking（含 compaction、reopen 和 leaf switch）；
  `model_change` 缺失 `modelId` 会被严格拒绝；
- Provider request 只接受完整且不可变的 Model，不存在 identity-only Model；ModelsStore 保存
  input、thinking map、cost tiers、context/max tokens 与 compat，并隔离/复制 JSON-like metadata；
  内置 `openai/gpt-5.5` 基线与原版 catalog 的名称、能力、上下文和分层价格一致；
- request-scoped stream options 已具备 typed fetch、payload/header/response hooks、header 三态
  删除和 thinking budgets；所有可显式设为零的数值 option 均区分“未设置”和“设置为零”；
  resolver 以 overlay 方式合并完整调用契约，provider header hook 位于最终 HTTP 边界；
  usage 始终含完整 cost object，并在所选 Model 边界统一计价；
- extension-neutral typed hook contract 覆盖 context、before agent start、provider request/headers/
  response、agent/turn/message/tool execution、model/thinking selection，以及 session start/shutdown/
  compact/tree/switch/fork；before-agent-start 可见完整 rich prompt、结构化 system prompt options
  和压缩后的 context；其中没有 JS loader、TUI 或 UI-only execution surface；
- 每个 terminal AssistantMessage 都带 provider/API/model provenance；response id/model、原始
  stop reason 和 diagnostics 在成功、失败、JSONL 重开及重新计价后均不丢失；失败消息还保留
  失败前已完成的 text/thinking/tool-call content，但不会把其中的 tool call 变成可执行终态；
- streaming message hook 和 observer 获得 canonical `assistantMessageEvent`、原始 stream event、
  response 起始时间以及包含当前 text/thinking/tool-call active block 的临时 AssistantMessage；
  partial 不会进入 provider context 或 durable history；
- compaction override 的 summary、first-kept entry、tokens、usage、details 与 from-extension 标记
  会进入同一个 durable commit，并保持 public start、before hook、commit、after hook、public end 顺序；
- deferred tools 不再只在 Request 收集：Responses 写入 client tool-search input，Kimi-compatible
  Completions 写入对应 system tool schema，普通 compat 路径不受污染；
- session tree 节点解析最新 label 与 label timestamp；Agent 和 AgentSession 的订阅面都是封闭的
  typed union，retry/compaction/queue 等低层控制事件不会混入原版 AgentEvent 生命周期；
- tool result/update 的 details 在工具、hook、每个 observer 与 durable state 之间做 JSON 验证和
  深复制，调用方后续修改不会反向污染事件或 session。

### P1–P2：AgentLoop 与 Agent 的边界尚未对齐

现有 `internal/agent` 已实现复杂的执行和状态协调，但“一次 run 的纯 Loop”与“长期
stateful Agent”的职责仍未按原版清楚分离。完整 `prepareNextTurn` context、
`shouldStopAfterTurn`、原版 queue delivery semantics、事件 payload 与 state 归并需要逐项
核对，而不是把现有类型直接改名后视为完成。

### P3：SessionManager 只有部分语义

底层 durability、tree 和 fork 能力值得保留，但 typed entry 主要集中在 message 与
compaction。model/thinking change、branch summary、custom entry/message、name/label/stats
以及与原版一致的 context 构建和 session navigation 尚未形成完整 SessionManager。

当前不能把“能够读写 JSONL”视为“与原版 session package 对齐”。旧 session 兼容也不是
后置 Provider 或 RPC 的附属工作，而是当前 Agent 核心数据模型的一部分。

### P4：AgentSession 尚未达到产品行为闭环

已有 AgentSession 包含 retry、compaction、queue 和动态 snapshot 等重要实现，但仍需按
原版重新校正职责并补齐：

- model/thinking change 的 durable entry 与完整恢复；
- resource/settings/auth reload 及其运行中传播；
- bash、当前 session tree navigation、name 和 stats；
- 精确 retry/compaction 控制面、context overflow 与失败恢复；
- 产品 event 和 extension-neutral hook/custom data；
- production factory 对上述 service、策略和动态依赖的完整装配。

### P5–P6：缺少内部 Runtime 与整体兼容验收

当前 executable 是一次性同步诊断路径，没有独立、长期、transport-neutral 的 application
Runtime，也没有 new/switch/fork/import 的 session replacement 生命周期。当前还缺少覆盖
原版完整 workflow 的跨层 golden scenario，因此局部 package 测试通过不能证明首期完成。

RPC 缺失是预期的后置项，不是当前要绕过核心架构抢先解决的 blocker。Provider breadth、
extension loader、pi-web 和 TUI 同样不在当前里程碑；但它们依赖的数据与 hook 契约属于
P0–P4，必须现在正确建模。

## 当前优先级

严格按照 [实现路线](ROADMAP.md) 推进：

1. P1：拆清并完整对齐 AgentLoop；
2. P2：完成 stateful Agent；
3. P3：完成 SessionManager；
4. P4：完成产品级 AgentSession；
5. P5–P6：建立进程内 Runtime 并做整体行为验收；
6. 之后才开始 RPC、Provider 扩展、pi-web 和其他 surface。

不为保留旧 package 或减少 diff 调整顺序，也不以 CLI demo 或单一 Provider 成功作为阶段
完成证据。本轮按约定在 P0 验收后停止，P1 尚未开始。

## 当前验证状态

P0 最终实现已通过原版对照、fixture/contract 测试及以下本地验证：

- `go build ./...` 通过；
- `go vet ./...` 通过；
- `go test -count=1 ./...` 通过；
- `go test -race -count=1 ./...` 通过。

旧 reasoning replay 断言已按原版完整 reasoning item（包括 summary）修正，因此不再是
常驻失败基线。P1 尚未实施。
