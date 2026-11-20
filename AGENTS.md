# pi-go 协作规则

## 项目目标

pi-go 是用 Go 编写的编程 Agent 运行时。Go 核心负责可持续交互的 Agent 和会话，各入口共用
这套能力。新增核心能力要验证完整的会话场景，不能以一次模型调用成功或页面能显示结果代替。

## 事实依据

以用户需求和 pi-go 的产品契约确定预期行为。修改前沿本仓库的生产调用链检查状态由谁持有、
事件何时发出、数据何时落盘。实现、文档和测试不一致时，先确认预期，不把现状直接当作规范。

不要把复现路径当作修复边界。定位根因，检查同一行为的其他路径，修复并验证实际受影响的部分。

## 核心架构

核心调用链保持为 `Runtime -> AgentSession -> Agent -> AgentLoop`，SessionManager 与
Session Store 保持独立职责：

- `AgentLoop` 负责一次运行中的 provider streaming、工具调度和事件序列，不拥有持久化、
  settings 或 session 生命周期；
- `Agent` 拥有长期 AgentState、prompt/continue、steering/follow-up queue、abort，并驱动
  AgentLoop；
- Session Store 负责可靠存储，SessionManager 负责 context、tree、branch、fork、compaction
  等 session 语义；
- `AgentSession` 组合 Agent、SessionManager 与应用服务，拥有重试、压缩、动态配置、reload、
  bash、产品事件和有序持久化；
- `Runtime` 负责服务装配、AgentSession 生命周期与 session replacement，并提供传输无关的
  command、state 和 event 边界。

不得为了减少改动而合并职责独立的层次、建立第二套核心状态，或把接入层逻辑下沉到核心。
新增 Provider 不应要求修改 AgentLoop；新增接入方式不应改变 message、session 或 event 语义。

## 对外能力边界

各入口通过核心 API 使用 Agent 和会话能力。协议编解码、命令转发、连接管理和事件传递属于
接入端；Agent/Session 状态、工具循环、队列、重试、压缩和动态配置由核心负责。接入端不得
合成核心事件或维护第二套会话状态。

核心 API 不依赖具体传输，不包含页面专用状态或 RPC framing。重构不能丢失模型、消息、工具结果、
会话条目或自定义数据中的信息，也不能改变 hook、事件顺序或持久化结果。有意变更契约时，
明确对调用者和已有数据的影响。

## Surface 架构

交互层按入口组织在 `surface/` 下，包括 CLI、TUI、Web、桌面和移动端。每个 Surface 可以拥有
自身的渲染状态、输入、HTTP/SSE/IPC、静态资源和专属测试，但不得拥有 Agent、Runtime 或
持久会话的权威状态。

多会话发现、打开去重、ApplicationSession/Runtime 生命周期和中立会话快照属于 `internal/application`；
`cmd/*` 只作为 composition root。Surface 共用 typed Application API 的 command、query、snapshot
和 event；HTTP JSON、SSE、终端事件或 GUI IPC 等传输编码留在对应 Surface。只有出现两个真实
消费者后才提取共享 wire protocol，禁止为了预想复用提前建立最低公分母交互抽象。

Web 日常开发使用 `make dev SURFACE=web`，由 Next HMR 代理 API-only Go 并自动重载 Go 服务，
不执行生产静态构建。`make build SURFACE=web` 与 `make run SURFACE=web` 保持分离；生产二进制
嵌入静态导出，运行时不依赖 Node。

## 文档维护

- README 负责入口与导航；架构文档说明职责、状态所有权、依赖与生命周期；命令、配置、协议和
  构建细节放在对应的领域或模块文档。
- 变化影响文档所述契约、用法，或使已有说明失真时，更新对应文档；内部优化、重构及保持
  契约的修复不要求追加文档。
- 直接修订已有段落，说明产品当前的行为和边界；详细说明集中在一个主要位置，其他文档概述或链接
  引用。优先复用现有文档，不为安放零散细节新增文档。
- 排查过程、临时性能数据和本次修改说明留在讨论或 PR；AGENTS.md 只记录可复用的协作规则，
  不堆积单次任务的例外。
- 不得在框架级别的文档补充技术细节，如果本次修改确实需要更新文档，必须和用户确认

## 验收与汇报

- 用回归样本和端到端场景验证产品契约，不以代码量、文件数、局部覆盖率或“已经能跑”作为完成证明；
- 测试可以使用 deterministic fake，production path 不得用占位代码、假数据或静态效果冒充
  真实能力；
- 修改后执行与风险相称的格式化、测试、race、vet 和 build 验证；
- 每次完成修改后，简要说明本次完成内容、验证结果和已知限制或未验证的部分。

## 协作底线

- 不相关修改属于用户，保留并忽略，不使用 Git 回退命令；
- 永远不要删除文件，确需移出仓库时移动到 `/tmp` 并说明位置；
- 搜索限于工作区或明确目录，不做全局 filesystem 搜索；
- 线性任务不委派；复杂任务需要委派时，禁止子 agent 继续派生。
