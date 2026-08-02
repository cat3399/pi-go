# pi 上游源码地图

本文记录 pi-go 当前上游基线的源码分布、产品依赖、热点文件和迁移边界。它回答的
是“pi 现在由什么组成”，不是“pi-go 必须复制什么目录”。规范性的 Go 架构仍以
[ARCHITECTURE.md](ARCHITECTURE.md) 为准。

所有数据对应 [UPSTREAM.md](UPSTREAM.md) 固定的 pi commit
`a116523434806910336b9de3e38a41aa5860030b`。推进上游基线时，必须在同一轮同步中
重新生成并审查本文；不能让新的迁移工作继续依赖过期的热点或依赖关系。

## 统计口径

- 统计各 production `src` 目录中的 TypeScript/TSX 文件。
- 不包含 test、fixture、文档、example、构建脚本、配置、asset 和预编译 binary。
- 行数是物理行，用来判断源码重心，不代表复杂度、完成度或预计 Go 行数。
- 495 个 TypeScript/TSX 文件共有 112,232 个物理行，其中 100,520 行非空。
- 文件名明确标记为 generated 的源码有 727 行；不能据此推断其他数据文件的生成
  来源，迁移时仍需逐项确认。
- 另外有 123 行 terminal native C 和 71 行 SQLite migration SQL；它们不改变整体
  分布，但属于跨平台和数据兼容清单。

源码行数只能用于识别调查成本和热点。功能是否完成，始终由行为、测试和 workflow
决定，不能用已翻译行数或文件数判断。

## Package 分布

| 上游 package | TS/TSX 文件 | 物理行 | 占比 | 主要职责 |
| --- | ---: | ---: | ---: | --- |
| `coding-agent` | 183 | 56,431 | 50.28% | CLI、session、tool、extension 和运行模式 |
| `ai` | 169 | 21,429 | 19.09% | AI 语义、API dialect、provider、auth 和 streaming |
| `tui` | 37 | 14,184 | 12.64% | terminal、editor、layout 和增量渲染 |
| `agent` | 37 | 10,368 | 9.24% | 低层 agent loop 与独立 AgentHarness 能力 |
| `server` | 30 | 4,281 | 3.81% | experimental server 能力 |
| `storage/sqlite-node` | 13 | 1,796 | 1.60% | AgentHarness 的 SQLite storage backend |
| `evals` | 8 | 1,277 | 1.14% | eval 工具 |
| `client` | 10 | 1,233 | 1.10% | remote session client |
| `protocol` | 8 | 1,233 | 1.10% | transport-neutral CBOR protocol |

`coding-agent`、`ai`、`tui` 和 `agent` 占全部 production TypeScript 的 91.25%。因此
迁移风险主要集中在产品编排、终端交互、provider 语义以及 agent/session 状态，而
不在 remote package 的文件数量。

从产品区域看：

- `coding-agent` 与 `tui` 共 70,615 行，占 62.92%；
- `ai` 与 `agent` 共 31,797 行，占 28.33%；
- server、storage、evals、client 和 protocol 共 9,820 行，占 8.75%。

这些区域不是 pi-go package 方案，只用于理解上游工作量所在。

## 上游 package 依赖与产品入口

固定基线的内部 package 依赖可以概括为：

~~~text
coding-agent executable
  |- agent -> ai
  |- ai
  |- tui
  |- client -> protocol
  `- protocol

server -> coding-agent + ai + protocol
storage/sqlite-node -> agent + ai
~~~

`coding-agent` 是用户直接运行的产品入口。它使用 `agent` 的低层 `Agent` 与类型，
同时在自己的 `core` 中维护 `AgentSession`、session persistence、tool、compaction、
settings、resource 和 extension orchestration。interactive mode 依赖独立的 `tui`
package；print mode 很薄，但仍依赖相同的 application/core 能力。

`client`、`protocol` 和 `server` 已在上游存在，不表示 pi-go 早期必须围绕 remote
boundary 构建。package dependency 也不等于目标 domain dependency；应先判断相关
能力在产品中的实际行为，再决定 Go 边界。

## 两条相邻的 agent 产品路径

固定基线中存在两组相邻但不等同的高层能力：

1. `packages/coding-agent/src/core/agent-session.ts` 及相邻的 session、tool、compaction
   和 product service，是当前 coding-agent executable 的主要产品路径。
2. `packages/agent/src/harness/` 提供独立导出的 `AgentHarness`，自身包含 session、
   tool、compaction、resource、environment 和 storage abstraction。

当前 `coding-agent` 源码直接使用低层 `Agent`，没有直接实例化 `AgentHarness`。两条
路径在 session、tool 和 compaction 上存在职责相邻或重叠的部分，但不能仅凭名称
判断它们应合并、替代或全部照搬。

阶段 0 必须把相关行为分别分类为：

- 当前 standalone coding-agent workflow 的直接依赖；
- pi 上游独立提供、但不在当前 CLI 主路径上的产品能力；
- 两条路径共享的稳定 domain invariant；
- TypeScript 实现组织或过渡结构，不应成为 Go 的重复实现。

在这项分类完成前，pi-go 不创建两套 session、tool 或 compaction runtime，也不以
任一上游 class 名称预设最终 Go 边界。

## 主要源码热点

热点表示需要优先调查和拆分，不表示应该优先整文件翻译。任何超过一个清晰行为
边界的文件，都必须先按 invariant 和 workflow 拆成 feature slice。

### coding-agent

`coding-agent` 共 56,431 行，内部重心如下：

| 区域 | 物理行 | 说明 |
| --- | ---: | --- |
| `src/core` | 27,951 | session、tool、extension、compaction 和 product service |
| `src/modes` | 18,862 | interactive 16,925 行、RPC 1,763 行、print 159 行 |
| `src/utils` | 3,291 | path、git、platform 和文本辅助逻辑 |
| `src` 根入口 | 3,121 | main、config、CLI、migration 和 package-manager CLI |
| `src/extensions` | 1,391 | extension 辅助能力 |
| `src/cli` | 1,224 | 参数和 selector |
| `src/client` | 536 | remote session |
| 其他 runtime 启动辅助 | 55 | 上游特定发布 runtime 的入口适配 |

`src/core` 继续拆分为：

- core 根文件 17,693 行；
- 内置 tool 4,109 行；
- extension runtime 3,893 行；
- compaction 1,510 行；
- HTML export 746 行。

最大的文件包括：

| 上游文件 | 行数 | 调查重点 |
| --- | ---: | --- |
| `packages/coding-agent/src/modes/interactive/interactive-mode.ts` | 6,125 | interactive lifecycle、command、render 和 session 协作 |
| `packages/coding-agent/src/core/agent-session.ts` | 3,332 | turn、queue、event、tool、retry 和 session orchestration |
| `packages/coding-agent/src/core/package-manager.ts` | 2,625 | package/resource 发现、安装和更新行为 |
| `packages/coding-agent/src/core/extensions/types.ts` | 1,713 | extension 暴露面和 TypeScript-specific contract |
| `packages/coding-agent/src/core/session-manager.ts` | 1,712 | session record、tree、保存、恢复和迁移 |
| `packages/coding-agent/src/core/settings-manager.ts` | 1,260 | settings source、优先级、更新与持久化 |
| `packages/coding-agent/src/core/resource-loader.ts` | 1,096 | prompt、skill、extension 和资源加载 |

这些文件是多个职责长期聚合的结果，不是合理的 Go 迁移单元。尤其不能把
`interactive-mode.ts` 或 `agent-session.ts` 分配为一次“翻译任务”。

### AI 与 provider

`ai` 共 21,429 行：

| 区域 | 物理行 | 占 package | 说明 |
| --- | ---: | ---: | --- |
| `src/api` | 10,587 | 49.40% | API dialect、payload、stream parser 和消息转换 |
| `src` 根文件 | 3,507 | 16.37% | 类型、model、registry、compat 和 generated data |
| `src/auth` | 3,366 | 15.71% | credential 与 OAuth flow |
| `src/providers` | 2,161 | 10.08% | provider registration、配置和 model metadata |
| `src/utils` | 1,763 | 8.23% | retry、validation、JSON、错误和 token 辅助逻辑 |
| `src/compat` | 45 | 0.21% | compatibility export |

主要 API hotspot 是：

- `openai-codex-responses.ts`：1,650 行；
- `openai-completions.ts`：1,523 行；
- `anthropic-messages.ts`：1,351 行；
- `bedrock-converse-stream.ts`：1,173 行；
- `openai-responses-shared.ts`：756 行；
- `mistral-conversations.ts`：677 行。

`providers` 有 83 个 TypeScript 文件，但总计只有 2,161 行，其中 deterministic
faux provider 单独占 541 行。大量 provider 文件只是薄 registration 或 metadata。
因此迁移和估算必须先按 API dialect 与行为族组织，再处理 provider matrix；不能
按 provider 文件数量建立同等大小的任务。

### TUI 与 interactive 产品面

`tui` 共 14,184 行，其中 component 为 5,200 行，其余 terminal、input、layout、
render 和 utility 为 8,984 行。主要 hotspot 是：

- `components/editor.ts`：2,351 行；
- `keys.ts`：1,401 行；
- `utils.ts`：1,303 行；
- `tui.ts`：1,223 行；
- `components/markdown.ts`：861 行；
- `TuiAltScreen.ts`：805 行；
- `autocomplete.ts`：786 行。

将 coding-agent interactive mode 的 16,925 行与 `tui` 的 14,184 行合并观察，
interactive 产品面至少有 31,109 行，占全部源码 27.72%。TUI 不是 CLI 外面的薄
client，而是 standalone 产品的重要组成；它可以晚于 headless 闭环，但最终必须
作为同一产品迁移和验收。

### Agent runtime 与 AgentHarness

`agent` 共 10,368 行：

- 低层 `Agent`、agent loop、types 和 proxy 为 2,260 行；
- `src/harness` 为 8,108 行，占 package 的 78.20%。

主要文件包括 `harness/agent-harness.ts` 1,185 行、`harness/types.ts` 980 行、
`harness/compaction/compaction.ts` 880 行、低层 `agent-loop.ts` 792 行。这里的行数
分布再次说明：迁移清单必须区分当前 coding-agent 主路径与独立 AgentHarness
能力，不能把 package 名称直接理解成一个单一的“agent core”。

### Remote、storage 与其他尾部

- `server` 为 4,281 行，其中 `legacy` 1,983 行；
- `storage/sqlite-node` 为 1,796 行；
- `protocol` 与 `client` 各为 1,233 行；
- `evals` 为 1,277 行。

它们体量较小，但 protocol compatibility、session durability、跨平台 server 生命周期
等行为未必简单。路线优先级依据产品依赖和风险，而不是仅按行数从小到大迁移。

## pi-go 的领域迁移模块

pi-go 使用领域模块组织架构，而不是复制上述 package。模块是职责、invariant 和
依赖的边界；初期不代表 public package，也不要求一个模块对应一个目录。

### 1. 基础语义

负责 message、content block、tool call、usage、finish reason、model metadata、
stream event 和稳定错误分类。它定义其他模块共享的最小语义，但不能成为容纳任意
类型的 common package。

主要上游证据来自 `packages/ai/src/types.ts`、model/stream utility、消息转换和
coding-agent 的扩展 message 定义。

### 2. AI 与 provider runtime

负责 provider authentication、request conversion、stream parsing、error mapping、
retry policy 以及后续 turn 必需的 vendor metadata。先用 deterministic fake 与一个
真实 API dialect 验证内部边界，再按行为族增加 adapter。

一个 provider 名称不是一个迁移模块；共享相同 wire behavior 的 provider 应复用
adapter 与 conformance suite，薄 registration 和 model data 独立管理。

### 3. Agent runtime

负责一次 turn 内的 model stream、tool-call loop、下一轮推理、steering/follow-up、
abort 和结束状态。核心 invariant 包括 event 顺序、单一 state owner、取消后的提交
边界，以及完成或失败时 goroutine 的生命周期。

它从低层 `agent-loop.ts`、`agent.ts`、当前 `AgentSession` 和 `AgentHarness` 中提取
行为证据，但不复制其中任一 class hierarchy。

### 4. Session 与 storage

负责 durable conversation state、append、tree、resume、branch、compaction record、
历史数据读取、迁移、恢复与写入一致性。domain state 与 storage record 必须分离；
unknown field、unknown event 和必要 vendor metadata 必须按兼容策略保留。

上游证据跨越 coding-agent `session-manager.ts`、AgentHarness session、JSONL record
以及 SQLite backend。相同 durable invariant 在 Go 中只实现一次，不因上游存在
多个 storage path 而复制 domain 规则。

### 5. Tool 与系统能力

负责内置 read、write、edit、bash 等 tool 的语义与执行生命周期，以及 filesystem、
subprocess、environment、root、output limit、timeout 和 cancellation policy。
platform-specific adapter 位于边界外侧，不能把 Node.js API 细节带入 domain。

### 6. Coding-agent application 与 headless CLI

负责把 agent、session、provider 和 tool 组合成用户 workflow，并提供配置装配、
命令入口、print/headless mode、退出码、signal 和诊断。application 可以依赖下层
模块，但下层模块不能反向依赖 CLI 或 terminal rendering。

首个纵向闭环应在此形成，而不是等待所有基础模块横向“迁移完”。

### 7. 产品服务

负责 model selection、auth storage、settings、system prompt、prompt template、skill、
resource loading、package management、context management、retry 和高级 compaction。
这些能力按真实 workflow 逐步加入，不预先建立覆盖所有未来状态的统一 service。

### 8. Interactive 与 TUI

负责 terminal lifecycle、input、editor、keybinding、autocomplete、layout、overlay、
incremental rendering、image、IME 和 selector。展示状态不进入 agent/session domain；
interactive mode 直接组合 application 能力，而不是通过为未来外部项目设计的 remote
API 间接使用 core。

### 9. Extension

在 standalone core 稳定后，从真实内部能力中提炼最小 extension surface。上游
extension types 和 loader 是需求证据，不是需要 source-compatible 翻译的 contract。
JavaScript module loading、plugin-specific DTO 和旧配置适配不进入核心模块。

### 10. Remote 能力

负责 pi 上游 protocol、client 和 server 中确属产品行为的部分。普通 storage
adapter 属于 Session 与 storage 模块；只有 transport-specific persistence 才随
remote 能力评估。迁移时重新决定 domain boundary、wire compatibility 和 transport；
不能因为上游已有 CBOR 或 server package，就让早期 standalone core 依赖 remote
architecture。

## 模块、slice 与 workflow

迁移使用三个不同层次，不能混用：

1. **领域模块里程碑是实现与 review 单位。** 它决定职责、invariant 和依赖方向，
   并把相关行为一起交付。
2. **Behavior feature slice 是追踪单位。** 它逐项记录可观察行为及其正常、错误、
   取消和数据路径，但不是独立停工点。
3. **完整用户 workflow 是验收单位。** 多个模块必须尽早组合成可执行闭环，避免
   各自完成后才发现集成语义不成立。

例如 Agent runtime 的首个里程碑可以把单轮响应、streaming、单 tool call、tool
failure、abort 和 settlement 一起实现并联合 review；连续 tool、retry 和 steering
留给后续模块里程碑。第一条产品 workflow 可以是：

~~~text
prompt
  -> deterministic fake provider stream
  -> agent turn
  -> one built-in tool
  -> assistant result
  -> session save and resume
  -> print mode exit
~~~

这条 workflow 同时经过多个模块，但每个模块只实现支撑当前行为所需的最小部分。
闭环稳定后，再扩充高级能力和 interactive 产品面。

## Module charter

开始实现任何领域模块前，必须写下或在 ledger 中关联以下内容：

- 模块负责与明确不负责的职责；
- 上游源码、测试、fixture、文档和 commit 依据；
- 输入、输出、错误、取消、并发和 durable data invariant；
- 与其他模块的依赖和 state ownership；
- TypeScript 与 Go 之间需要重新决策的语义差异；
- 首批 behavior slice 及其验收 workflow；
- deferred 和 intentionally-incompatible 项目的重新评估条件。

Module charter 可以随已验证行为演进，但变更必须说明受影响的 slice 和数据。它不是
public API 承诺，也不能成为提前冻结整个 core 的理由。

## 热点文件的使用规则

- 热点文件用于发现职责和 invariant，不直接作为迁移任务。
- 调研时围绕一个行为读取相关源码、调用者、测试、fixture 和历史说明，不能只读
  单个大文件后推断完整语义。
- 如果一个行为跨多个上游 package，应在同一个 feature slice 中保留端到端证据，
  而不是按 package 分别声明完成。
- 如果多个行为集中在一个上游文件，应拆成多个 slice；不得让“文件已翻译”成为
  behavior ledger 的状态。
- AI/provider 按 API dialect 和行为族组织；TUI 按 input、layout、render 等语义组织；
  session/tool/compaction 按 state invariant 组织。
- 上游同步时先比较模块和行为影响，再更新行数与热点；行数变化本身不触发机械迁移。
