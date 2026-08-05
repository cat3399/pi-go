# 实现路线

路线按依赖关系推进内部核心，不以尽快接上 pi-web 为优化目标。阶段编号表示验收顺序；
实现可以并行准备，但下一层不能用临时抽象绕过上一层尚未解决的语义。

当前是快速重构期：允许调整 package、类型和文件布局，也允许替换已有实现。目标不是最小
diff，而是尽早消除会扩散到后续 Runtime、RPC 和 pi-web 的错误边界。

## P0：对齐兼容数据模型

先把后续所有层共享的语义从原版完整移植：

- 完整 `Model`、provider/API 标识、能力、cost、compat 与 stream options；
- `AgentMessage` union、`convertToLlm` 和 custom/bash/compaction/branch 消息；
- tool execution result 的 content、details、usage、added tool names、terminate，以及最终
  ToolResultMessage 的 identity、`isError` 和 timestamp；
- Agent/Provider/tool lifecycle event、usage、cost、stop/error reason；
- v3 session entry union、context、tree/branch 与 custom data；
- 与核心相交的 hook 输入、输出和取消语义。

这一阶段冻结的是语义与序列化行为，不是 Go public API。Provider breadth 暂不扩展；使用
现有两个 OpenAI dialect 和一个完全不依赖 OpenAI metadata 的 fake 验证抽象。

**验收门槛**：原版 fixture 能无信息丢失地解码/编码或转换；Model、Message、ToolResult、
session entry 和 event 有对照测试；通用测试不导入 vendor wire type。

## P1：AgentLoop 行为对齐

将单次 active run 收敛为独立 AgentLoop：

- 完整 streaming、tool scheduling、rich result 与 multi-turn loop；
- `prepareNextTurn` 的完整 context、steering 注入和 `shouldStopAfterTurn`；
- 精确 message/tool/event 顺序，并逐 turn 保留 provider 返回的 usage/cost；AgentLoop 不跨
  turn 聚合 usage；
- partial stream、provider/tool failure、abort 和 settlement；
- 每个 provider turn 消费调用方提供的最新不可变 snapshot。

**验收门槛**：用 deterministic fake 覆盖 text、thinking、image、并行/顺序工具、动态工具、
stop hook、失败和 abort；Loop 不依赖 session、settings 或具体 Provider。

## P2：stateful Agent 行为对齐

按原版建立独立 `Agent` 层：

- 完整 AgentState 与状态变化事件；
- prompt/continue、唯一 active run、subscribe、abort 和 wait；
- steering/follow-up queue 及 delivery mode；
- turn snapshot、消息归并和可重入边界；
- model、thinking、system prompt 与 tools 的运行中变更。

**验收门槛**：同一 Agent 可连续运行；tool chain 的下一 turn 能看到动态变更；queue mode、
重复调用、abort/settlement 和 observer 重入均有行为测试。

## P3：SessionManager 行为对齐

在可靠存储之上完成原版 session 语义：

- append-only v3 entry 与当前 leaf；
- context 构建、tree/branch/fork 与分支选择；
- compaction、branch summary、model/thinking change；
- name、label、custom entry/custom message 和 session listing metadata；
- legacy/unknown data 的明确兼容策略与 round-trip 保护。

**验收门槛**：原版 session fixture 在 Go 中得到相同的 tree、当前分支和 LLM context；重启、
fork、压缩与损坏恢复不会静默改写或丢失数据。

## P4：产品级 AgentSession 行为对齐

用 Agent、SessionManager 和应用服务组装 coding-agent 核心：

- prompt/continue/steer/follow-up/abort 与有序持久化；
- retry、Retry-After、context overflow 和 automatic/manual compaction；
- model/thinking 的控制与 durable change entry，tool/system prompt 的动态更新；
- bash、resource/settings/auth reload 和动态工具；
- session navigation、name、stats 与产品 event；
- extension-neutral hook 和 custom data 通路。

**验收门槛**：partial stream 后失败、工具已产生副作用、重试与取消竞争、压缩写入失败、
运行中 reload、重启续聊等组合场景都保持 memory、durable state 与 event 一致。

## P5：进程内 Application Runtime

建立与 transport 无关的长期装配层：

- 统一创建 Model/Settings/Auth/Resource 服务和 AgentSession；
- new/switch/fork/import session 的替换与清理；
- 通过 in-process Go API 暴露完整命令结果、state snapshot 和 event；
- 明确进程退出、active run、flush 与资源释放顺序。

**验收门槛**：测试进程可仅调用 Go API 完成原版核心 workflow；production assembly 不再
遗漏 retry、compaction、dynamic reload 或 session service，也不依赖 RPC framing。

## P6：内部核心验收

在开始 RPC 前做一次整体兼容审查，而不是把缺口留给前端发现：

- 与原版运行同一组 golden scenario/fixture；
- 覆盖连续对话、rich input、工具返回图片、切模、thinking、queue、压缩、fork 和恢复；
- 核对完整 event 序列、usage/cost、session JSONL 与错误分类；
- 运行 unit、integration、race、build 和 vet，并记录所有有意差异及其 Go 理由。

只有 P0–P6 全部通过，才称为“Agent 首期完整重写”。CLI 能运行一次 prompt、某个 Provider
可用或 pi-web 能显示文本，都不能替代这个门槛。

## 首期之后

后续按以下顺序推进，任何一项都不得反向引入第二套核心状态或前端专用语义：

1. 原版兼容的 JSONL RPC 编解码与长期进程控制；
2. Go/TypeScript 跨实现 contract fixture 和协议测试；
3. 补充 Provider adapter 与 model catalog breadth；
4. extension/plugin loader、外部桥接与完整 hook 集成；
5. pi-web 启动和 transport 的小范围切换；
6. TUI 与其他 surface。

Provider adapter、extension loader 和 transport 可以后置，它们所依赖的 Model、Message、
session、event 与 hook 契约不能后置。
