# 架构边界

## 架构中心

pi-go 的中心是 standalone coding agent 产品，而不是 remote API。CLI、TUI 和
其他内置运行模式直接使用同一套 Go application 与 domain 能力，不要求经过
network 或 serialization boundary。

初始依赖方向如下，具体目录名称可以随实现演进：

~~~text
            pi-go executable
          /        |         \
       CLI     print mode     TUI
          \        |         /
        coding-agent application
                   |
          agent / session runtime
             /       |       \
           AI       tool    storage
             \       |       /
        provider and platform adapters
~~~

上层负责产品 workflow，下层负责可复用语义和外部系统接入。依赖方向不能因为
未来某个 plugin 或 pi-web 的需求而倒置。

## 迁移组织模型

pi-go 不按上游文件、package 或 class 机械迁移。迁移工作使用三个层次：

1. **领域模块里程碑是实现与 review 单位**：一次完成一组相互关联的职责、invariant、
   state ownership 和依赖方向，再做联合审查。
2. **Behavior feature slice 是追踪单位**：记录每个可观察行为及其正常、错误、取消
   和数据路径，但不要求逐项停工或独立 review。
3. **完整用户 workflow 是验收单位**：尽早把多个模块组成可执行闭环，不能等各
   模块分别“完成”后才验证集成。

一个领域模块可以跨越多个上游 package，也可以由多个 Go internal package 实现。
反过来，一个上游热点文件通常包含多个行为，必须拆成多个 slice。源码行数、文件
存在或编译通过都不是模块完成条件。

开始实现模块前，需要建立 module charter，至少记录：

- 负责和明确不负责的职责；
- 上游源码、测试、fixture、文档与固定 commit 依据；
- 输入、输出、错误、取消、并发和 durable data invariant；
- 依赖方向、state ownership 和允许的并发边界；
- TypeScript 与 Go 之间需要重新决策的语义；
- 首批 behavior slice 和验收 workflow。

Module charter 是当前设计假设，可以随行为证据演进，不是 public API 或 wire
compatibility 承诺。上游源码分布、热点与这些模块的证据映射见
[SOURCE_MAP.md](SOURCE_MAP.md)。

## 初始领域模块

下面的模块是迁移起点，不预设最终目录名称。

### 基础语义

承载 message、content、tool call、usage、finish reason、model metadata、stream
event 和稳定错误类别。只共享已经稳定且被多个真实场景需要的语义，不能演变为
无边界的 common package。

### AI 与 provider runtime

承载 provider auth、request conversion、stream parsing、error mapping、retry 和
必要 vendor metadata。Provider adapter 位于 domain 外侧；先由 deterministic fake
与一个真实 API dialect 验证边界，再按行为族增加 adapter。

### Agent runtime

承载 turn lifecycle、model stream、tool-call loop、下一轮推理、steering/follow-up、
abort 和结束状态。它依赖基础语义、provider、tool 与 session，但不拥有 terminal
展示或持久化 record 格式。

### Session 与 storage

承载 durable conversation state、append、tree、resume、branch、compaction record、
历史格式兼容、恢复和写入一致性。Domain state、storage record 与具体 filesystem/
SQLite adapter 分离，相同 durable invariant 只实现一次。

### Tool 与系统能力

承载 read、write、edit、bash 等内置 tool 的语义和执行生命周期。Filesystem、
subprocess、environment 与 platform adapter 位于边界处；root、permission、timeout、
output limit 和 cancellation 是模块 contract 的一部分。

### Coding-agent application 与 headless CLI

组合 agent、session、provider 与 tool，形成 prompt、stream、tool execution、保存、
恢复和退出的用户 workflow。CLI、print mode、signal、exit code 和诊断属于这一层；
下层 domain 不反向依赖命令行或 serialization。

### 产品服务

按实际 workflow 提供 model selection、auth storage、settings、system prompt、prompt
template、skill、resource loading、package management、context management、retry 和
高级 compaction。不同能力不因同属“配置”或“资源”而被塞进巨大统一 service。

### Interactive 与 TUI

承载 terminal lifecycle、input、editor、keybinding、autocomplete、layout、overlay、
incremental rendering、image、IME 和 selector。Interactive mode 直接组合 application
能力，展示状态不得进入 agent/session domain。

### Extension

在 standalone core 稳定后，从真实内部能力中提炼最小 extension surface。上游
extension type 和 loader 是需求证据，不是 source-compatible contract。

### Remote 能力

承载上游 protocol、client 和 server 中确属产品行为的部分。普通 storage adapter
属于 Session 与 storage 模块；只有 transport-specific persistence 才随 remote 能力
评估。进入该模块时重新决定 domain boundary、wire compatibility 和 transport；
早期 core 不得为了它建立统一 remote API。

初始依赖主线可以概括为：

~~~text
              CLI / print / interactive TUI
                         |
              coding-agent application
                  /              \
          product services    agent runtime
                              /     |      \
                    AI/provider    tool   session/storage
                              \     |      /
                              base semantics

           extension and remote capabilities are later boundaries
~~~

## Go package 策略

- 迁移早期的大多数实现默认放在 `internal` 范围，保留调整 package 和类型的空间。
- interface 应由实际使用者需要的行为定义，并尽量小；不为每个 TypeScript class
  创建对应 interface。
- 共享类型只承载确实稳定的语义，不能形成无边界的 common package。
- 错误应保留可判断的类别和 cause，同时提供清楚的用户信息。
- `context.Context` 用于调用生命周期、deadline 和 cancellation，不作为任意数据
  容器，也不能被长期保存。
- 只有被多个真实场景反复使用且职责稳定的能力，才考虑提升为 public package。

上游的 `ai`、`agent`、`coding-agent`、`tui`、`protocol`、`server` 和 `client`
package 是迁移清单，不是必须照抄的 Go module 结构。

固定上游基线还同时包含 coding-agent 自己的 `AgentSession` 产品路径和 `agent`
package 导出的独立 `AgentHarness`。二者在 session、tool、compaction 与 resource 上
存在相邻职责，当前 coding-agent executable 仍直接使用低层 `Agent`，没有直接
实例化 `AgentHarness`。阶段 0 必须先分类两条路径的产品行为和共享 invariant；
不得仅因上游有两套 class，就在 Go 中预设两套 runtime。

## Agent 与 session

Agent loop 负责 model stream、tool call 和下一轮推理的控制流程；session 负责
可持久化的对话状态和恢复语义。二者可以协作，但不能把 provider 的临时对象直接
作为 durable state。

每个活跃 session 必须有明确的 state ownership。provider stream 和 tool 可以
并发执行，但所有影响 session 的变化必须按可解释的顺序提交。取消后遗留的
goroutine 不得继续修改已结束的 turn 或 session。

Steering、retry、compaction、branch、resume 等高级行为应在基本 agent loop 和
session invariant 稳定后逐步加入，不能一开始设计一个覆盖所有未来状态的巨大
state machine。

## AI 与 provider

内部 message、content、tool call、usage 和 finish reason 只表达 pi-go 所需语义，
不复制任一 vendor SDK 的全部类型。

每个 provider adapter 负责认证、请求转换、stream parsing、错误映射以及必要的
vendor metadata 保留。跨 provider 的 normalize 必须有 fixture 和 regression
test；不能为了统一表面类型而丢失后续 turn 需要的数据。

Provider interface 应在至少一个真实 adapter 和一个 deterministic fake 中得到
验证，再根据新增 provider 的差异演进。

## Tool 与系统能力

内置 tool 是 pi-go 产品功能，不依赖 extension system 才能工作。read、write、
edit、bash 等能力应直接实现并测试，其 root、environment、output limit、timeout
和 cancellation 必须明确。

Tool 执行不能隐式扩大进程已有权限。项目 trust、credential 和工作目录规则属于
产品安全边界，需要在 CLI 与非交互模式中保持一致。

## CLI、TUI 与运行模式

CLI 参数解析、interactive mode、print/headless mode 和 TUI 都属于 standalone
验收范围。它们可以共享 application service，但展示状态和 terminal rendering
不能进入 agent domain。

TUI 按键、布局、宽度计算、增量渲染、图片和输入法等行为需要独立测试。为了先打通
agent 闭环，可以先交付 print mode，但不能把 TUI 永久降级为外部 client。

## 持久化与兼容数据

现有 pi session 和配置数据属于用户迁移能力，应在 core 阶段考虑，而不是归入
plugin 或 pi-web 兼容。

- domain state 与 storage record 分离，转换位置明确。
- reader、writer 和 migration 分别测试。
- 需要 round-trip 的 unknown field 或 record 必须保留。
- corrupt、partial write 和不支持的版本必须返回可诊断错误，不能 silent truncate。
- 写入策略必须考虑 crash consistency 和多进程访问。

## Extension surface

Extension boundary 不在迁移初期预先设计。完成 core 后，先调查现有 extension
实际使用了哪些能力，再从已经稳定的内部语义中提炼最小边界。

pi-go 的责任是提供通用功能、清晰生命周期和可靠错误语义。现有 plugin 如何映射
自己的 API、如何兼容旧配置、是否需要独立 adapter，由 plugin 或其维护者决定。

不同能力可以采用不同方式：某些场景适合 Go interface，某些场景可能需要受控的
subprocess 或 protocol。除非届时的需求和测试支持，不统一预设一种 extension
机制，也不嵌入 JavaScript runtime。

## 外部集成与 transport

pi-web 和其他外部项目在 standalone pi-go 完成后接入。接入时先识别它们所需的
通用能力，再决定是否复用上游 protocol、增加独立 adapter，或采用其他进程边界。

任何公开 transport 都必须与内部 domain 解耦，但“解耦”不等于现在就必须创建
统一的 remote API。HTTP、SSE、WebSocket、CBOR、stdio 和 Unix socket 当前都
不是既定方案。

pi-go 不接受仅服务某个页面、frontend state 或 pi-web workflow 的字段和分支。
如果外部项目需要转换，应由其 client、BFF 或独立 adapter 完成。

## Public API 与版本控制

初期内部 API 可以随迁移调整。一个能力成为 public package、extension contract
或 wire protocol 后，才需要明确 versioning、compatibility window 和 breaking
change policy。

公开边界必须有自己的 conformance test，并记录调用者应如何处理新增字段、未知
event、取消、错误和版本不匹配。不能用过早冻结整个 core 的方式换取未来兼容性。
