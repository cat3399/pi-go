# 近期路线

路线只描述影响整体方向的阶段，不再为每个小模块维护迁移 charter 和状态台账。每个
阶段通过端到端行为测试验收，而不是通过文件数量或局部测试数量验收。

## 1. 修正 Agent 核心边界

AgentSession 是本阶段的组织中心。Model/Provider/Message 与它高度相关，应在同一轮
核心重构中完成：

1. 从原版实际实现和测试中移植通用 Model、Message、Provider contract；
2. 将当前 Agent 中的一次运行控制流拆成 AgentLoop；
3. 建立长期存在的 AgentSession，拥有动态配置、conversation、queue 和 active run；
4. 在每次 provider turn 前准备最新 model、thinking、system prompt 和 tools snapshot；
5. 保持现有 event order、tool scheduler、取消和 durable commit 能力。

验收场景至少包括：

- 同一个 session 连续处理多个 prompt；
- tool chain 中下一轮能够看到新配置；
- 运行期间切换 model/thinking 后，下一次推理使用新值；
- abort、provider failure 和 tool failure 后 session 状态仍然一致；
- 进程重启后可以从 durable conversation 继续。

## 2. 贯通富内容

将 text/image/tool result 从 AgentSession 一直贯通到 Provider adapter 和持久化：

- AgentSession 的 prompt、steer 和 follow-up 接受 rich user message；
- tool result 保留 provider-visible content 和 runtime-visible details；
- context transform、compaction 与 session codec 不丢失图片；
- OpenAI Responses adapter 先证明链路，通用接口不暴露 OpenAI wire type。

验收以真实的“图片输入”和“工具返回图片后继续推理”场景为准。

## 3. 接入已有能力

在 AgentSession 生命周期中接入已经存在的实现：

- retry 与 Retry-After；
- threshold/context-overflow/manual compaction；
- steering/follow-up queue 与 continue；
- model/settings/resource reload；
- usage、cost 和运行事件。

重点测试组合语义：partial stream 后失败、tool 已产生副作用、压缩写入失败、取消与重试
竞争，以及进程结束前的 durable settlement。

## 4. 建立长期 Runtime 与 pi-web 接入

实现长期运行的 application runtime，并优先兼容原版已有的 JSONL RPC 命令与事件：

- prompt、steer、follow_up、abort；
- set_model、set_thinking_level、compact；
- new/switch/fork session；
- 规范化 message、tool、usage、error 和 lifecycle event。

随后修改 pi-web 的启动和 transport adapter，使其使用 pi-go。只有通用 AgentSession
能力确实缺失时才修改 core，不加入页面专用状态。

验收是 pi-web 的核心 workflow 不再实例化 TypeScript AgentSession，并能仅依赖 pi-go
完成对话、工具、切模、压缩和 session 操作。

## 5. 后续补全

核心与 RPC 闭环稳定后再处理：

- 原版旧 session 文件的完整语义兼容与迁移；
- 更多 Provider 和 model catalog；
- 完整 interactive TUI；
- plugin、extension 和其他外部集成；
- release、跨平台和长期运行完善。

这些工作不得反向改变已经验证的 AgentLoop、AgentSession 和通用 Provider 边界。
