# 迁移路线

迁移主线遵循两个原则：先做依赖少、行为明确、测试容易移植的部分；尽早形成一个
真实可用的纵向闭环。任何阶段都只运行 Go 实现，不以 TypeScript runtime 补齐
尚未迁移的功能。

阶段编号表达依赖关系，不要求每个阶段只能发布一次。每个阶段可以包含多个完整的
领域模块里程碑。

## 迁移组织方式

所有阶段同时使用三种粒度：

- 领域模块里程碑定义职责、invariant、state ownership 和依赖方向，也是实现与
  独立 review 单位；
- behavior feature slice 用于拆解可观察要求、测试和 ledger，不强制拆成独立实施阶段；
- 完整用户 workflow 是跨模块验收单位。

领域模块可以作为一个完整实现任务，但不能把上游目录或大文件机械翻译成所谓模块。
一个里程碑可以跨越多个上游 package 并包含多个相关 behavior；完成后统一 review，
再接入纵向 workflow。领域模块、上游热点及其证据映射见 [SOURCE_MAP.md](SOURCE_MAP.md)。

每个模块编码前建立简明 module charter，并确认该里程碑覆盖的上游证据、行为 contract
和测试裁判。实现过程中按组件自检；整个里程碑关闭正常、错误、取消和数据路径后，
更新 ledger 并做一次联合 review。上游大文件只用于调查，不直接成为任务或完成度单位。

每个领域模块里程碑还要经过独立 review gate。Reviewer 不能是该里程碑的实现者，
并且不能只复查测试是否通过；还要检查上游证据与 charter 是否一致、依赖方向和
state ownership 是否清楚、是否引入会阻塞后续 slice 的 abstraction 或兼容债务，
以及 deferred 项目是否有明确的重评条件。存在未关闭 blocker 时不能扩大下游实现。

## 阶段 0：建立事实基线

- 固定 pi 上游 commit，记录 package dependency 和可执行入口。
- 生成并审查上游源码地图、热点文件和 package/domain dependency。
- 建立 behavior ledger、test ledger、数据格式清单和 provider matrix。
- 区分产品行为、实现细节、测试工具、example、eval 和 extension 相关内容。
- 记录 session、配置、credential、model catalog 等需要兼容的数据来源。
- 分类当前 coding-agent `AgentSession` 路径与独立 `AgentHarness` 路径的直接产品
  行为、共享 invariant、独立能力和实现组织细节。
- 为基础语义、provider、agent、session、tool、application 等首批领域模块建立
  module charter，并画出第一个 standalone workflow 的依赖链。
- 为每项清单保留上游文件、测试名称和 commit 依据。

这一阶段只确认事实和范围，不定义 public API、extension protocol 或 pi-web
接入方式。

退出条件：可以从清单中选择首批 feature slice，能说明其所属模块、依赖、行为
证据、state ownership 和测试来源，并能说明这些 slice 如何进入第一个完整用户
workflow。

## 阶段 1：Go 基础与低耦合能力

- 初始化 Go module、基本目录、构建和 CI。
- 建立 deterministic fake provider、clock、ID、filesystem 和 test fixture。
- 建立基础语义模块，迁移 message/content、纯数据转换、stream event 和稳定错误
  分类，但不为尚未出现的消费者扩张共享类型。
- 迁移 model metadata、配置解析、路径处理以及其他低耦合纯逻辑。
- 可以并行纳入 TUI 字符宽度、按键解析等独立且测试清晰的算法，但不因此提前
  固化整体 TUI 架构。

每项实现都与对应测试一起迁移。公共类型保持最少，优先验证 Go 表达是否合理。

退出条件：基础模块可以独立测试，fixture 可重复，默认测试不需要 Node.js、
network 或真实 credential。

## 阶段 2：最小 standalone 闭环

- 实现最小 agent loop 和 deterministic fake provider。
- 完成至少一个真实 provider adapter 的基本 streaming 路径。
- 支持 prompt、assistant stream、tool call、abort 和错误结束。
- 实现最小 session 创建、追加、保存和恢复。
- 实现至少一个满足真实 workflow 的内置 tool，并明确 root、output limit、timeout
  和 cancellation。
- 通过 print/headless mode 提供第一个可使用的 pi-go executable。

该阶段不建设 plugin system、remote API 或 pi-web adapter。

首个验收 workflow 应至少覆盖：prompt 输入、deterministic provider stream、agent
turn、一次 tool execution、assistant result、session 保存与恢复，以及 print mode
退出。随后再用真实 provider adapter 的隔离验证证明 request/stream boundary 成立。

退出条件：上述包含 model 调用、tool 执行和 session 持久化的 E2E scenario 完全
运行在 Go 中，进程树中不存在 TypeScript pi；相关模块的职责和 state ownership
已经由该 workflow 验证，而不是只由各自 unit test 推断。

## 阶段 3：补齐 coding agent core

- 按领域模块和 behavior slice 扩展能力，不以完成某个上游目录为目标。
- 完善内置 read、write、edit、bash 等 tool 及其错误、限制和取消行为。
- 迁移 session resume、branch、compaction、恢复和历史数据兼容。
- 迁移 model 选择、auth、settings、system prompt、prompt template 和 skill。
- 支持 steering、thinking、retry、usage、context management 和复杂 tool flow。
- 按行为优先级增加 provider，并保留必要的 vendor-specific metadata。
- 补齐 print mode、非交互 CLI 和核心诊断能力。

`agent-session.ts`、`session-manager.ts`、provider API hotspot 等上游聚合文件必须按
行为和 invariant 拆分。若一个新 slice 迫使多个模块同时改变 ownership 或依赖
方向，先修订 module charter 并审查设计，再编码。

退出条件：不依赖 interactive TUI 的主要 coding agent workflow 已由 pi-go 独立
完成，对应 regression、race 和恢复测试通过。

## 阶段 4：完成 interactive 产品

- 迁移 interactive mode 和 TUI 组件。
- 迁移输入、编辑、autocomplete、keybinding、overlay、layout 和增量渲染。
- 覆盖 Unicode、CJK、terminal capability、resize、图片和跨平台差异。
- 补齐 CLI 参数、session picker、model/config selector 和其他用户入口。
- 建立可安装 executable、版本信息、升级策略和 release smoke test。

Interactive mode 与 TUI 作为一个跨模块产品面验收，但实现仍按 input、editor、
keybinding、layout、overlay、render、terminal lifecycle 等行为切片推进。不得把
上游 `interactive-mode.ts`、editor 或 TUI 主类作为一次翻译任务。

退出条件：pi-go 可以作为日常使用的独立 coding agent，核心 interactive workflow
通过 terminal integration test 和人工 smoke test。

## 阶段 5：完成 pi 上游范围

- 补齐剩余 provider、auth、tool、session 和高级行为。
- 按其在 pi 产品中的实际作用迁移 storage、protocol、client 和 server。
- 迁移适用于 Go 实现的 package test、integration test、E2E test 和历史 fixture。
- 完成 Linux、macOS、Windows 的行为、构建和并发验证。
- 补充性能、长时间运行、故障恢复和升级测试。
- 关闭 behavior ledger 和 test ledger 中的所有未分类项目。

迁移 `protocol/server/client` 是因为它们属于 pi 上游功能，不是为了提前服务
pi-web；是否保留上游边界和 wire format，要在迁移该功能时依据行为与兼容需求
决定。

退出条件：满足 PROJECT.md 的 standalone 和完整迁移验收标准。

## 阶段 6：提炼 extension surface

- 调查上游 extension 对 tool、provider、session、command、event、UI hook 等
  能力的真实使用方式。
- 区分应成为稳定 core 能力的部分与应由 adapter 处理的兼容细节。
- 从已稳定的 Go 实现中提炼最小 public surface，并补充 conformance test。
- 根据不同场景评估 Go interface、subprocess、RPC 或其他边界。
- 定义必要的 lifecycle、permission、isolation、error 和 versioning 规则。

pi-go 不负责模拟 Node.js module loading，也不保证现有 plugin 无修改运行。plugin
维护者或独立 adapter 负责接入 pi-go 提供的通用能力。

退出条件：至少由真实 extension 场景验证公开能力足够且没有反向污染 core，公开
边界有明确版本和测试。

## 阶段 7：外部项目集成

- 重新分析届时版本的 pi-web 需求和现有依赖方式。
- 优先在 pi-web 内建立适配边界，使用 pi-go 已公开的通用能力。
- 只有通用能力确实缺失时才扩展 pi-go，不能加入 pi-web-specific behavior。
- 根据部署和运行需求选择合适 transport，不继承早期假设。
- 让约定的 pi-web workflow 仅使用 pi-go，并通过 E2E test。

其他 plugin、extension 或外部项目遵循相同原则：pi-go 提供能力，消费者负责兼容。

## 持续的上游同步

每次有意推进 pi 上游基线时：

1. 在独立变更中记录旧 commit 和新 commit。
2. 生成源码、测试、数据格式和依赖差异。
3. 更新 SOURCE_MAP.md 的 package 分布、热点、产品路径和模块证据映射。
4. 编码前先按领域模块分类新增、改变和删除的行为。
5. 更新 behavior ledger、test ledger、module charter 和受影响的 fixture。
6. 实现差异，或明确记录暂缓与有意不同的理由。
7. 清单内部一致后，再更新 docs/UPSTREAM.md。

外部项目的更新不与 pi core 基线混在同一轮处理。
