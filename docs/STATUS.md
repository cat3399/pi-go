# 当前状态

本文是 2026-08-03 的实现快照，基于代码、测试和 production assembly 重新审查，不沿用
旧迁移计划的完成度判断。

实现审查基线：

- pi-go：`3b39253e2b5547e05d206941fcc2feccc90f62ae`
- 原版 pi：`a116523434806910336b9de3e38a41aa5860030b`
- pi-web：`dfab5853b8d2f717df259e7ebc94f49a3c2e43e7`

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

- `Model` 现包含 request-wide cost tiers、完整的 portable stream options、OpenAI、Anthropic
  与 Bedrock 的 typed compat，以及保留未实现 API compat 的 immutable raw projection。未实现
  adapter 的 compat 可以读取/复制，但 production route 会明确拒绝，绝不假装已经消费它；
- `internal/agentmsg` 提供可扩展的 AgentMessage union 和唯一的 `ConvertToLLM` 边界。标准
  LLM、bash、custom、branch summary、compaction summary 和 opaque extension message 不会提前
  降成字符串；
- ToolResult 的 rich content、details、usage/cost、added tool names、terminate、identity、
  `isError`、timestamp 已贯通 tool execution、event copy、session JSONL codec 和 replay；
- v3 session 的 message、thinking/model change、compaction、branch summary、custom/custom
  message、label、session info 都有 typed payload，并能 append、reopen、branch/context projection；
- P0 对照测试覆盖 coding-agent message conversion、原版 v3 entry JSON shapes、metadata
  round-trip 和不含 OpenAI metadata 的 generic provider contract。

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

1. 拆清 AgentLoop 和 stateful Agent；
2. 完成 SessionManager；
3. 完成产品级 AgentSession；
4. 建立进程内 Runtime 并做整体行为验收；
5. 之后才开始 RPC、Provider 扩展、pi-web 和其他 surface。

不为保留旧 package 或减少 diff 调整顺序，也不以 CLI demo 或单一 Provider 成功作为阶段
完成证据。

## 当前验证基线

P0 完成后的验证基线：

- `go build ./...` 通过；
- `go vet ./...` 通过；
- `go test ./...` 通过；
- `go test -race ./internal/agent ./internal/session ./internal/provider ./internal/model ./internal/agentmsg` 通过。

旧 reasoning replay 断言已按原版完整 reasoning item（包括 summary）修正，因此不再是
常驻失败基线。P1 开始前，当前默认测试集没有已知豁免。
