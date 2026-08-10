# 后续开发计划

本文只保留尚未完成且已经排定优先级的工作。已完成能力记录在 `docs/STATUS.md`，WebUI 的独立
功能账本记录在 `docs/WEBUI.md`，不在路线中重复维护历史阶段。

## 当前目标：AgentSession/Runtime 跨实现验收

当前唯一主线是建立固定原版版本的 TypeScript/Go 黑盒场景，并由场景差异驱动 Agent 核心修复。
基线为 `../pi` 的实际生产链：

`AgentSessionRuntime → AgentSession → Agent → runAgentLoop → SessionManager`

原版 `AgentHarness` 仍在演进且尚未取代 coding-agent 生产链；只跟踪已经稳定并进入生产调用方的
契约，不提前把 pi-go 改写为另一套未落地架构。

首个 `multi_turn_rich_tool_reopen` 场景已经固定并通过，覆盖 rich input、工具调用/结果、连续
prompt、Provider 输入、完整事件、最终状态/统计、JSONL 与 reopen。当前从队列/abort 场景继续推进。

### 第一批共同场景

1. steer/follow-up delivery mode、clear queue、abort 与 settled 竞态；
2. provider retry、Retry-After、context overflow、自动/手动 compaction；
3. model、thinking、active tools、system prompt 与 reload 的 turn snapshot；
4. branch、tree navigation、fork 和损坏 session 恢复。

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
- reload 后 settings/resources/tools/system prompt 的完整一致性。

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
