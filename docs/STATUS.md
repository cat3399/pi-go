# 当前状态

本文记录 2026-08-02 对 commit `8ab9029` 的实现审查结论。结论来自当前 Go 代码、测试和
实际 production assembly，不沿用旧迁移台账中的“已完成”判断。

参考版本：

- pi-go：`8ab9029145905a5785ed9851a7c6086db9df9e4f`
- 原版 pi：`a116523434806910336b9de3e38a41aa5860030b`
- pi-web：`dfab5853b8d2f717df259e7ebc94f49a3c2e43e7`

## 总体判断

pi-go 目前是一个质量较好的“OpenAI Responses + 内置工具 + durable transcript”的单次
Agent 内核，但还不是完整 Pi 的 Go 重写，也还不能让 pi-web 只做少量修改后获得完整
功能。

问题不在于所有已有代码都要重做，而在于产品级 AgentSession 层缺失，导致底层能力
没有形成长期运行、动态可配置的 Agent 产品闭环。

## 能力概览

| 区域 | 当前已经存在 | 主要缺口 |
| --- | --- | --- |
| Application | `-p` headless 入口、signal、stdout/stderr、session 打开与创建 | 只有一次同步运行；没有 interactive 或长期 RPC runtime |
| Agent | streaming、连续 tool loop、多 tool 并行/顺序、abort、continue、steer/follow-up queue、retry/compaction 机制 | 配置在 `New` 后固化；职责混合；没有产品级 AgentSession |
| Provider | deterministic scripted provider、OpenAI Responses text/thinking/tool stream 与 replay | production 只允许 OpenAI Responses；通用类型带有 OpenAI 形状 |
| Model | settings/models catalog、provider/model 选择、部分 metadata | Agent 实际只拿到最小 `ModelRef`；能力和 request options 没有贯通 |
| Message | text、thinking、tool call/result 和 image 的部分 value type | Agent 入口与队列只收 string；tool adapter 只返回 text；富内容未贯通 |
| Session | JSONL v3、原子追加、锁、恢复、tree/branch/fork、compaction、legacy import、unknown raw 保留 | 只语义化 message/compaction；不能完整恢复 model/thinking/branch summary 状态 |
| Tool | production 已装配 bash、read、write、edit、grep、find、ls | registry 的结构化 details 在 Agent adapter 中丢失；缺少 session/runtime 级动态管理 |
| Auth/Resource | API key、OpenAI OAuth、settings/model/resource/prompt 加载与 trust boundary | 主要在进程启动时读取，不能由长期 AgentSession 动态刷新 |
| TUI | terminal/input/text 的基础实现和测试 | 未进入 executable，距离原版 interactive 产品仍很远 |

“代码已经存在”不等于“产品入口已经具备该能力”。当前最明显的例子是：

- production 创建 Agent 时没有提供 context window、summarizer 和 compaction 参数，自动
  压缩不会启用；
- retry controller 存在，但 production 没有提供完整策略或用户控制面；
- steer/follow-up、continue 和 manual compact 没有长期 runtime 可以调用；
- rich message 类型存在，但 `Run`、`Steer`、`FollowUp` 与工具输出仍是文本接口；
- TUI package 存在，但 main executable 只接受 `-p`。

## 主要架构偏差

### 1. 缺少 AgentSession

当前 `internal/agent.Agent` 同时承担部分 loop、队列、重试、压缩和 session 协调，却又
不能在运行期间更新 model、thinking、system prompt 或 tools。它位于一个尴尬的中间层：
对纯 AgentLoop 来说职责过多，对产品 AgentSession 来说能力不足。

这是下一阶段必须先修正的问题。

### 2. 通用层受 OpenAI Responses 约束

`provider.ModelRef` 主要是 route identity，`RequestOptions` 只表达 tools 与并行开关；
同时 `llm` 通用层保存 OpenAI Responses replay 类型。继续沿这个边界增加 Provider，
会迫使每个新 adapter 改动核心消息或 Agent 逻辑。

下一版直接移植原版通用 Model、Message、Provider 语义，不另造一套抽象。

### 3. 富内容没有端到端闭环

图片底层类型已经存在，所以不是从零开始；真正缺少的是输入、队列、工具结果、上下文、
持久化、provider 和 runtime event 的统一通路。这属于核心消息模型，不能留到 UI 阶段
再补。

### 4. Session 是存储兼容，不是完整行为兼容

现有存储的 durability 和 unknown-data 处理值得保留。完整原版 session 语义可以暂缓，
但当前还不能据此宣称旧会话可无差别恢复运行状态。

### 5. 已实现能力没有全部装配

这一部分不需要重新研发，但要放到正确的 AgentSession 生命周期内接入，并验证 retry、
tool side effect、partial stream、compaction commit、queue 和 cancellation 的组合顺序。

## 当前优先级

现在暂停扩展 Provider 数量、完整 TUI、plugin/extension 和精确旧 session 兼容。近期只
围绕以下结果推进：

1. 建立真正的 AgentSession，并收窄 AgentLoop；
2. 移植原版 model/provider/message 核心语义；
3. 贯通图片和富工具结果；
4. 把 retry、compaction、queue、resource reload 等现有能力接入；
5. 提供长期 RPC runtime，再验证 pi-web 的最小改动接入。

不使用源码行数、文件数量、模块版本号或 ledger 状态衡量完成度。完成度只由完整行为
场景证明。

## 当前验证基线

当前基线下以下检查通过：

```sh
go test ./...
go vet ./...
go build ./...
go test -race ./internal/agent ./internal/session ./internal/provider
```

这些结果证明现有模块的局部质量，不代表已经达到原版 Pi 的端到端行为兼容。
