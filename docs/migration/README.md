# 迁移执行台账

本目录把 [ROADMAP.md](../ROADMAP.md) 的阶段计划落实为可以逐项核验的迁移状态。
它不按上游文件统计“完成度”，只记录领域模块、behavior slice、上游测试、持久化
格式和完整 workflow 的证据与结论。

所有上游引用默认指向 [UPSTREAM.md](../UPSTREAM.md) 固定的 pi commit
`a116523434806910336b9de3e38a41aa5860030b`。引用必须包含仓库相对路径，并尽量给出
symbol、test suite 或 test case 名；仅写一个目录或热点文件名不算充分证据。

## 工件

- [BEHAVIORS.md](BEHAVIORS.md)：可观察行为、所属模块、依赖、状态和实现证据。
- [TESTS.md](TESTS.md)：上游测试意图及 `ported`、`strengthened`、`deferred` 等结论。
- [DATA_FORMATS.md](DATA_FORMATS.md)：session、配置、credential 和 catalog 等 durable data。
- [PROVIDERS.md](PROVIDERS.md)：provider、API dialect、认证与 model catalog 行为族。
- [AGENT_PATHS.md](AGENT_PATHS.md)：低层 Agent、AgentSession 和 AgentHarness 的职责分类。
- [SCOPE.md](SCOPE.md)：product、library、extension、test、fixture、example、eval 和实现细节分类。
- [WORKFLOWS.md](WORKFLOWS.md)：跨模块产品验收 workflow。
- [REVIEWS.md](REVIEWS.md)：独立模块审查及未关闭事项。
- [modules/README.md](modules/README.md)：首批领域模块的 module charter。

这些文件共同构成状态，不能只更新其中一张表。一个模块里程碑进入实现时，要确认
所含 behavior、相关 upstream test、module charter 和 workflow；完成并联合 review
后再统一更新状态。

## 稳定 ID

- 模块：`M-BASE`、`M-PROVIDER`、`M-AGENT`、`M-SESSION`、`M-TOOL`、`M-APP` 等。
- 行为：`B-<MODULE>-NNN`，例如 `B-AGENT-002`。
- 上游测试意图：`T-<MODULE>-NNN`。
- 数据格式：`D-<AREA>-NNN`。
- Workflow：`WF-NNN`。
- 独立审查：`R-<MODULE|STAGE>-NNN`，例如 `R-BASE-001` 或
  `R-STAGE0-001`。

ID 一旦进入实现、测试或 review 记录就不复用。行为拆分时保留原 ID 并注明被哪些
新条目替代，避免历史审查失去指向。

## 状态语义

Behavior 使用以下状态：

- `classified`：上游证据与边界已确认，尚未开始实现；
- `in-progress`：实现、测试和台账正在同一个 slice 中推进；
- `ported`：行为、测试、quality gate 与独立 review 全部通过；
- `deferred`：有明确依赖和重新评估条件，不能作为永久 skip；
- `intentionally-incompatible`：差异已经明确批准并记录数据或用户影响；
- `not-applicable`：仅属于 TypeScript/runtime/packaging 实现细节，没有产品行为。

Test ledger 的最终状态严格使用 [TESTING.md](../TESTING.md) 规定的 `ported`、
`strengthened`、`deferred`、`intentionally-incompatible` 和 `not-applicable`。正在实现
的 test intent 在完成前仍保持 `deferred`，并把 active slice 写入重评条件；这样不会
把尚未通过的测试提前记为已迁移。

Data inventory 使用 `classified`、`deferred` 和 `baseline-artifact-gap`。前两项分别
表示来源/兼容要求已取证、或等待明确消费者再实现；`baseline-artifact-gap` 表示固定
commit 中可读取生成产物，但缺少精确重建该产物所需的原始输入或 manifest。它不是
“可以忽略”，必须保留缺失内容、影响和重新评估条件。

## 独立 review gate

领域模块按 charter 中声明的里程碑逐步完成，不等待该模块所有未来高级行为一次性
迁完。每个里程碑在扩大下游实现前都必须经过独立 reviewer，流程是：

1. 实现者关闭该里程碑声明的正常、错误、取消和数据路径，运行适用 quality gate，
   并更新 behavior、test、workflow 与数据清单。
2. Reviewer 读取完整 diff、module charter 和精确上游证据，不能只依据实现者摘要。
3. Reviewer 分别审查行为正确性、规则符合性、依赖与 state ownership、并发和数据
   安全，以及现有设计是否能自然承载已知后续 slice。
4. 结论写入 [REVIEWS.md](REVIEWS.md)。未关闭 blocker 时，相关 behavior 不能标成
   `ported`，依赖它的模块也不能扩大实现范围。
5. 非 blocker 只能在记录影响、owner module 和重新评估条件后延期；临时对话或
   “以后再看”不构成记录。

如果发现必须改变项目硬约束、破坏兼容数据或引入生产 fallback 才能继续，应视为
严重阻塞并与项目负责人确认，不能静默降级。
