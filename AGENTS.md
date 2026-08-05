# pi-go 协作规则

## 项目目标

pi-go 的目标是用 Go 尽可能一比一移植 `../pi` 的完整 Agent 能力，不是复刻大致思路，
也不是制作功能相似但语义缩水的实现。产品核心必须独立运行于 Go，不启动、代理或
fallback 到 TypeScript pi。

先完成完整、可长期驱动的 Go Agent 核心，再建立 transport 和具体接入。CLI 能运行、单次
模型调用成功或页面能显示结果，都不能替代核心能力完成。

## 事实依据

原版 pi 的实际代码、调用链、类型、行为和测试是最高依据，其次是可跨实现复用的 fixture
与端到端 scenario。pi-go 的既有实现、测试和文档只能作为迁移材料，不能反过来定义原版
语义；旧测试通过也不能成为保留错误架构或行为偏差的理由。

修改前应调查原版完整调用链和状态所有权。不要根据孤立类型猜测架构，也不要臆造原版
不存在的约束，再以此为由降低能力。

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

不得为了减少改动而合并原版独立层次、建立第二套核心状态，或把接入层逻辑下沉到核心。
新增 Provider 不应要求修改 AgentLoop；新增接入方式不应改变 message、session 或 event 语义。

## 对外能力边界

Go 核心必须对外提供与原版一致的完整 Agent 能力和可观察语义。pi-web 或其他接入者只负责
协议编解码、命令转发、连接管理和事件传递，不得代替核心维护 Agent/Session 状态、执行工具
循环、实现 queue/retry/compaction、合成事件，或补偿 session 与动态配置能力。

核心 API 必须保持 transport-neutral，不包含页面专用状态或 RPC framing。接入者只做薄适配，
不重新实现半套 Agent。

## 移植范围与原则

Go 与 TypeScript 语言机制不同的部分可以采用合理的 Go 写法，包括类型表示、并发、错误和
资源生命周期；这些调整不得删除信息、改变状态所有权、事件顺序、持久化结果或其他可观察行为。

TUI、UI 交互、extension/plugin loader 和外部桥接可以不移植，但它们依赖的 Model、Message、
ToolResult、session entry、event、hook 与 custom data 等核心契约不得缩水。未经用户明确要求，
不得为了快速实现而简化原版逻辑。

## 验收与汇报

- 使用原版行为、fixture 和完整 scenario 验证兼容性，不以代码量、文件数、局部 coverage 或
  “已经能跑”作为完成证明；
- 测试可以使用 deterministic fake，production path 不得用占位代码、假数据或静态效果冒充
  真实能力；
- 修改后执行与风险相称的格式化、测试、race、vet 和 build 验证；
- 每次完成修改后，简要说明本次完成内容、验证结果以及仍与原版存在的差异。

## 协作底线

- 不相关修改属于用户，保留并忽略，不使用 Git 回退命令；
- 永远不要删除文件，确需移出仓库时移动到 `/tmp` 并说明位置；
- 搜索限于工作区或明确目录，不做全局 filesystem 搜索；
- 线性任务不委派；复杂任务需要委派时，禁止子 agent 继续派生。
