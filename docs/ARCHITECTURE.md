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
