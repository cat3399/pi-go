# 核心架构

## 目标

pi-go 重写的是 pi 的完整 Agent Runtime。兼容范围包含：

- 能力：原版 Agent 核心能力不缩水；
- 分层：各层职责、状态所有权和调用方向对应原版；
- 行为：消息、工具、队列、重试、压缩、取消、持久化和事件时序一致；
- 数据：Model、AgentMessage、ToolResult、session entry、event 和 hook payload 不丢信息。

代码形式不要求逐行翻译。Go 特有的锁、goroutine、context、接口和错误包装可以自由设计，
前提是不会改变上述语义。

## 对齐基线

对齐工作以固定上游版本中产品实际使用的生产调用链为准：

`AgentSessionRuntime → AgentSession → Agent → runAgentLoop → SessionManager`

上游实验性抽象只有进入实际产品调用方后才成为迁移基线，不能用未落地的新接口跳过
AgentSession 的 retry、overflow recovery、resource/tool 管理或 `agent_settled` 等现有语义。

主要参考入口：

- `../pi/packages/agent/src/agent-loop.ts`
- `../pi/packages/agent/src/agent.ts`
- `../pi/packages/agent/src/types.ts`
- `../pi/packages/coding-agent/src/core/agent-session.ts`
- `../pi/packages/coding-agent/src/core/agent-session-runtime.ts`
- `../pi/packages/coding-agent/src/core/session-manager.ts`
- `../pi/packages/coding-agent/src/core/sdk.ts`

## 目标调用链

```mermaid
flowchart TB
    TUI["TUI surface"] --> API["application.API"]
    CLI["CLI surface"] --> API
    GUI["GUI surface"] --> API
    Browser["Browser WebUI"] --> Web["HTTP/SSE adapter"]
    Web --> API
    RPC["RPC / automation adapter"] -.-> AppSession["ApplicationSession"]
    API --> Service["Application Service"]
    Service --> AppSession
    AppSession --> Runtime["Application Runtime"]
    Runtime --> Services["Model / Settings / Auth / Resource"]
    Runtime --> Session["AgentSession"]
    Session --> Agent["Agent"]
    Session --> Manager["SessionManager"]
    Manager --> Store["Durable session storage"]
    Agent --> Loop["AgentLoop"]
    Loop --> Provider["Provider contract"]
    Loop --> Tools["Tool runtime"]
```

Surface 和外部 transport 只驱动 Application API/Runtime，不拥有第二套 Agent 状态。HTTP、SSE、进程内
调用或 JSONL 只是接入方式，不能在接入层重新实现 queue、retry、compaction 或 session 行为。
Application Service 是多会话生命周期层，不把多个 Runtime 合成一个可变全局状态；每个
活动会话仍由自己的 Runtime/AgentSession 权威拥有。

## 分层职责

### AgentLoop

AgentLoop 负责一次 active run：

- 调用 Provider 并转发 streaming/message/tool lifecycle；
- 执行工具批次并保留完整 ToolResult；
- 处理 multi-turn、steering/follow-up 注入和下一 turn snapshot；
- 处理 provider/tool failure、partial stream、abort 和 settlement。

它不拥有 durable session、产品设置、retry、compaction 或长期队列。

### Agent

Agent 是通用 stateful wrapper，拥有：

- system prompt、model、thinking、tools、messages 和 streaming 状态；
- 唯一 active run、listener、abort 和 wait；
- steering/follow-up 队列及 delivery mode；
- AgentLoop event 到 AgentState 的归并。

Agent 不知道 session 文件、resource 目录、pi-web 或具体 Provider。

### SessionManager

SessionManager 是持久会话树的语义层，负责：

- append-only session entry 和当前 branch/leaf；
- context 构建、tree、branch、fork 和恢复；
- compaction、branch summary、model/thinking change；
- name、label、custom entry/message 和 listing metadata；
- legacy/unknown 数据的明确兼容策略。

底层存储负责锁、追加、原子发布和故障恢复，不能取代 SessionManager 的语义。

### AgentSession

AgentSession 是 coding-agent 产品核心，负责：

- 组合 Agent、SessionManager 和应用服务；
- prompt/continue/steer/follow-up/abort；
- 有序持久化、retry、overflow recovery 和 compaction；
- system prompt、tool registry/active tools、model/thinking 和 reload；
- standalone bash、session navigation、name、stats 和产品事件；
- extension-neutral hooks 与 custom data 通路。

AgentSession 拥有产品策略，但运行中的 model、thinking、system prompt、tools 和 messages
必须以 Agent state 为唯一事实源。

### Application Runtime、ApplicationSession 与 Service

Runtime 负责创建 cwd-bound 服务和 AgentSession，以及 new/switch/fork/import/dispose 等
replacement 生命周期。

ApplicationSession 在 Runtime 之上提供单会话的 transport-neutral 边界：

- command dispatch 和结果；
- 权威 state snapshot；
- 单会话有序 event stream；
- session/runtime replacement identity 与生命周期；
- active operation、flush 和 shutdown 顺序。

Application Service 在 ApplicationSession 之上提供多会话发现、打开去重、空闲回收、权威
snapshot，以及跨会话的全局 revision/cursor event stream。Transport adapter 只进行 framing、
字段投影和连接管理。

### Surface 与 Application Service

CLI、TUI、WebUI 和 GUI 是 Application API 的独立 surface adapter：

- 共用强类型 command、query、event 和 capability contract；
- 只保留滚动、面板、窗口、选中标签等瞬时呈现状态；
- 不读取 session 文件推导实时 Agent 状态；
- 可以按各自媒介投影、合并或编码事件，但不能改变 durable 或 canonical 事件语义；
- 不要求共用按钮、对话框、主题或其他最低公分母 UI 抽象。

多会话 surface 通过 Service 持有 ApplicationSession/Runtime 句柄并统一收束生命周期。TUI 可以只
暴露单会话体验，WebUI/GUI 可以多标签，但底层 Agent 行为和状态所有权不分叉。

`cmd/pi-go` 是唯一 composition root，通过 `run`、`web`、`rpc` 子命令装配不同 adapter。
GUI/TUI/CLI 默认与 Application Service 同进程；浏览器使用版本化 HTTP/SSE；JSONL RPC 只保留为
外部自动化和兼容验收 transport，不作为进程内产品 surface 之间的桥。

## 核心不变量

- AgentSession、Agent 和每次 active run 都有唯一状态 owner。
- 每个 provider turn 使用不可变 snapshot，turn 之间能看到最新配置。
- assistant tool call 在工具执行前已经进入 Agent state 和持久化顺序。
- 并行工具可以按完成顺序发事件，但 ToolResult message 按原调用顺序进入上下文。
- `agent_end` 只表示一次底层 run 结束；高层操作完成以 `agent_settled` 为准。
- `agent_settled` 对外可见时 session 已经 idle。
- active tools、tool registry、system prompt 和 resources 由同一条重建路径保持一致。
- abort 或 shutdown 后，遗留 goroutine 不得继续提交 message、entry 或 event。
- 通用层不依赖 vendor wire type，durable schema 不静默丢弃未知数据。
- fake 只用于测试，不能进入普通 production assembly。

## 范围边界

下列能力属于独立 Surface 或产品集成，可以在不分叉 Agent Core 的前提下独立演进：

- TUI、主题和终端交互；
- JS extension/plugin loader 与 extension custom UI；
- HTML export 和其他 surface-specific 功能；
- Provider 数量扩展。

下列契约属于 Agent Core，任何 Surface 的开发顺序都不能成为缩减它们的理由：

- vendor-neutral Model/Provider/Message/ToolResult 契约；
- AgentSession 产品行为和事件语义；
- extension-neutral hooks、custom data、skills/templates 和 trust；
- transport 所依赖的权威 state、command result 和 event。
