# 项目章程

## 使命

pi-go 将成为 pi 产品的完整、整洁、可维护的 Go 实现。Go 在项目中是一等公民：
产品结构、并发模型、错误处理、测试方式和发布方式都应按照 Go 的特点设计。

迁移可以循序渐进，早期版本也可以缺少高级能力，但不能通过双 runtime、长期
兼容层或外部项目专用逻辑换取表面上的功能完整。

## 首要目标

### 独立的 pi-go 产品

首要交付物是能够独立运行的 pi-go，而不是供某个外部项目调用的一组 library
或 service endpoint。长期迁移范围包括：

- model、provider、message、streaming 和 usage 等 AI 能力；
- agent loop、tool calling、state management 和 cancellation；
- coding agent 的内置 tool、session、配置、auth、model 选择、上下文管理、
  compaction、prompt、skill 和运行模式；
- CLI、interactive TUI、print/headless 等用户可直接使用的产品表面；
- pi 上游自身提供的 storage、protocol、client 和 server 能力，但它们按照产品
  依赖和优先级迁移，不作为早期 core 的前置架构。

上游 package 列表只用于梳理范围，不要求 pi-go 复制相同的 package 边界。

迁移组织遵循三层口径：领域模块里程碑是实现与 review 单位，behavior slice 是
需求和测试追踪单位，完整用户 workflow 是跨模块验收单位。模块负责定义职责、
invariant 和依赖方向；一个里程碑应把相互关联的行为一起实现，再做一次联合 review；
多个模块仍要尽早组成真实闭环。固定基线的源码分布、热点和候选模块见
[SOURCE_MAP.md](SOURCE_MAP.md)。

### 完整的回归保护

- 每项迁移功能都必须同时迁移相关测试，不能先堆积实现、最后补测试。
- 每个相关上游测试都必须进入 test ledger，并有明确结论。
- 在 Go 中风险更高的并发、parser、持久化和资源管理路径，应增加 race、fuzz、
  fault injection 和恢复测试。
- TypeScript 实现可以在隔离环境中作为 test oracle，但不能成为默认测试和产品
  运行的必需依赖。

### 可持续跟随上游

- 每轮迁移和同步都基于明确记录的 pi commit。
- 新增、改变和删除的上游行为先分类，再进入实现。
- 已迁移、暂缓、有意不同和不适用的项目始终可查询、可追溯。

## 后续目标

### Extension 能力

只有在 standalone pi-go 的主体功能稳定后，才从真实 core 中识别适合 plugin
或 extension 使用的能力。pi-go 负责提供完整、通用、可组合的功能与稳定语义；
具体 plugin 的数据转换、调用适配和历史兼容由 plugin 自身或独立 adapter 负责。

是否使用 Go interface、subprocess、RPC 或其他边界，需要根据届时的隔离、性能、
部署和语言需求单独决策。当前不预设 language-neutral protocol，也不承诺现有
JavaScript/TypeScript plugin 的 source compatibility。

### pi-web 集成

pi-web 是 core 完成后的下游集成目标，不是 pi-go 的设计中心。进入该阶段后，
应优先修改 pi-web，使其适配 pi-go 已经形成的通用能力。pi-go 不增加页面、React、
Next.js 或 pi-web workflow 专用逻辑。

pi-web 仍属于整个项目的最终验收范围，但不参与早期 package、public API 或
transport 的定义。

## 验收标准

### Standalone 验收

- pi-go 可以独立完成纳入当前阶段的完整用户 workflow。
- 已声明支持的功能全部由 Go 实现，production 不启动或依赖 TypeScript 版 pi。
- 对应的 Go unit、integration、regression 和 E2E test 全部通过。
- 历史 session 等需要兼容的数据能够安全读取；任何无法支持的差异都有明确记录。

### 完整迁移验收

- behavior ledger 和 test ledger 中没有未分类项目。
- pi 基线内属于产品行为的功能均已迁移，或有经过确认的有意差异。
- CLI、TUI、agent、provider、tool、session 以及纳入范围的其他产品表面通过测试。
- 跨平台、并发、取消、恢复和升级路径达到可发布要求。
- 上游同步流程可以由文档和工具重复执行，不依赖个人记忆。

### 外部集成验收

- pi-go 提供经过真实使用验证的通用 extension surface，而不是某个 plugin 的
  专用兼容层。
- 需要接入的 plugin 或 extension 能够通过自身修改或 adapter 使用这些能力。
- pi-web 经自身适配后，仅使用 pi-go 完成约定 workflow，并通过对应 E2E test。

外部集成验收位于 standalone 和完整迁移之后，不能反过来阻塞或扭曲 core。

## 硬约束

### Go 是唯一产品实现

允许用固定版本的 TypeScript pi 做行为对照，但产品路径不得导入、启动、proxy
或 fallback 到它。迁移过程不建设需要长期维护的双端路由。

### 先迁移产品，再冻结边界

早期实现默认保持内部可重构。只有经过多个真实功能使用、职责稳定的能力，才适合
成为 public package、extension API 或 wire protocol。不能为了想象中的消费者
提前固化 abstraction。

### 行为兼容，不机械翻译

兼容目标是用户可观察行为、数据语义和重要错误行为，不是文件数量、源码行数、
npm import path 或 class 层次。TypeScript abstraction 不适合 Go 时，应保留其
行为和 invariant，重新设计实现。

上游热点文件通常聚合多个职责，只用于定位行为证据，不作为迁移任务。一个行为
跨越多个上游 package 时，应在同一个 slice 中保留端到端语义；一个文件包含多个
行为时，应拆成多个 slice。任何模块开始实现前都要明确 module charter，但这不
构成 public package 或长期兼容承诺。

### 数据安全

- 持久化格式必须有明确的读取、写入和迁移策略。
- 无法理解的数据不能被静默截断或覆盖。
- corrupt、partial write 和版本不兼容必须返回可诊断的错误。
- 格式变更需要 fixture、round-trip 和升级测试。

### 安全边界

credential、tool 执行、文件系统、subprocess、network 和 remote resource 都是
trust boundary。secret 不得进入 log、fixture、event 或 error payload。默认
测试使用 fake，不使用真实 credential。

## 当前明确不做

- 为了早期功能覆盖率引入 TypeScript/Go production fallback。
- 为 pi-web 设计专用 endpoint、DTO、状态分支或 package。
- 在 core 尚未稳定时决定 HTTP、SSE、WebSocket、CBOR 等外部 transport。
- 复刻 npm package 的 import 兼容性或嵌入 JavaScript runtime。
- 承诺现有 plugin 无修改运行。
- 对整个仓库进行一次性机械翻译。

## 决策优先级

目标冲突时，依次考虑：

1. 正确性、数据安全和可恢复性。
2. standalone pi-go 的产品完整性。
3. 清晰、符合 Go 习惯的实现。
4. 测试覆盖和上游可追溯性。
5. 长期可维护性、安全性和可观测性。
6. 有数据支持的性能。
7. 外部项目接入的短期便利。
