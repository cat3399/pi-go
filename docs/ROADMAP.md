# 后续开发计划

本文只保留尚未完成且已经排定优先级的工作。已完成能力记录在 `docs/STATUS.md`，WebUI 的独立
功能账本记录在 `docs/WEBUI.md`，不在路线中重复维护历史阶段。

## 当前目标：AgentSession/Runtime 跨实现验收

当前唯一主线是建立固定原版版本的 TypeScript/Go 黑盒场景，并由场景差异驱动 Agent 核心修复。
基线为 `../pi` 的实际生产链：

`AgentSessionRuntime → AgentSession → Agent → runAgentLoop → SessionManager`

原版 `AgentHarness` 仍在演进且尚未取代 coding-agent 生产链；只跟踪已经稳定并进入生产调用方的
契约，不提前把 pi-go 改写为另一套未落地架构。

`multi_turn_rich_tool_reopen`、`queue_clear_abort_settled`、`provider_retry_after_recovery`、
`manual_compaction_reopen` 和 `context_overflow_compact_continue` 已经固定并通过，覆盖 rich input、
工具循环、多轮上下文、混合 queue mode、queue recall、abort 后续跑、Agent retry、手动/自动压缩、
overflow continue、最终 settled、完整事件、JSONL 与 reopen。`turn_snapshot_model_tools_reload` 也已
逐字段通过，固定了运行中请求不可变、同一 run 下一 tool turn 刷新 model/thinking/tools/prompt，
以及 reload 后资源 prompt 和 durable state 的语义。`tree_navigation_runtime_fork` 已逐字段固定
无摘要 tree navigation、废弃分支保留、Runtime session replacement、fork `parentSession`、源与分支
双 JSONL 及 reopen。`damaged_session_resume_continue` 也已固定坏行保留、orphan 根投影、恢复分支的
初始化选择、真实 Provider 续写和再次打开。当前进入图片过滤、thinking budget 与资源装配场景。

### 下一批共同场景

1. `images.blockImages`、`thinkingBudgets` 与额外 prompt/skill production resource 路径；
2. retry/compaction、abort、reload 和 tree navigation 的复杂竞争组合。

每个场景必须同时比较：

- 发给 Provider 的 system prompt、消息、工具 schema 和 stream options；
- 完整 AgentSession 事件顺序及关键 payload；
- command/result 与权威 state snapshot；
- Session JSONL entry、parent 关系、context/tree 投影；
- usage/cost、finish reason、错误分类和 shutdown 结果。

时间、随机 ID 和临时绝对路径只允许在比较层做确定性归一化；不得删除事件、消息字段或失败结果
来制造通过。测试可使用 deterministic Provider 控制输入，但 production assembly 仍必须使用真实
Provider/工具。完成一个高内聚场景组后，再用 DeepSeek 做一次真实短程只读验收。

## 由等价场景驱动的修复

共同场景发现不一致后，按原版状态所有权和调用链修复 Go 实现，不在 fixture 中接受 pi-go 现状。
当前已知、但仍需由场景覆盖的 Agent 可观察差异包括：

- `images.blockImages` 的全上下文图片过滤；
- settings `thinkingBudgets` 到每次 Provider turn 的动态传递；
- 额外 prompt/skill 路径进入 production resource assembly；
- reload 后动态 settings 与额外 production resource 路径的完整一致性。

Provider/API/Auth 数量扩展、完整 JS extension/plugin runtime、extension custom UI 和 WebUI 功能不作为
当前 Agent 等价验收的前置条件。相关核心数据若已经存在于 session/message/event 契约中，仍必须
无损保存；暂缓的是宿主和产品覆盖面，不是允许缩减通用类型。

## 完成标准

只有生产链的共同场景全部通过，并且没有依靠 Surface、RPC 或静态 fixture 补偿核心状态，才可以
宣布 Agent 重写完成。单元测试数量、一次 CLI/网页成功回复或单一真实模型调用都不能替代该标准。

## 参考源码

- `../pi/packages/agent/src/agent.ts`
- `../pi/packages/agent/src/agent-loop.ts`
- `../pi/packages/agent/src/types.ts`
- `../pi/packages/coding-agent/src/core/agent-session.ts`
- `../pi/packages/coding-agent/src/core/agent-session-runtime.ts`
- `../pi/packages/coding-agent/src/core/session-manager.ts`
- `../pi/packages/coding-agent/src/core/sdk.ts`
