# 测试与回归策略

测试迁移与功能迁移是同一项工作。不能先翻译大量源码，再把上游 test suite 当作
最后的验收工具。每个模块里程碑先确认行为证据，再把相关测试和实现一起完成，
最后统一更新 ledger 并 review。

默认 Go test suite 必须 self-contained，不依赖 Node.js、network、真实 provider
credential 或上游 checkout。

## 上游测试的定位

TypeScript test suite 是行为证据和回归历史，不要求逐行翻译测试代码。迁移时需要
保留测试意图、输入边界、可观察结果和曾经修复的 bug。

每个相关上游测试必须归入一种状态：

- `ported`：Go test 直接覆盖相同测试意图；
- `strengthened`：由更完整的 table-driven、property、fuzz、race 或 integration
  test 覆盖；
- `deferred`：依赖尚未迁移的功能，并记录具体依赖和计划阶段；
- `intentionally-incompatible`：行为差异经过确认，并记录理由和影响；
- `not-applicable`：只验证 TypeScript runtime、npm packaging 或其他实现细节，
  不包含需要保留的产品行为。

`deferred` 不是永久 skip。每个条目都需要上游文件、测试名称或 fixture、固定
commit 和重新评估条件。

## 模块里程碑流程

每个领域模块里程碑按以下顺序完成：

1. 阅读相关源码、测试、fixture 和历史回归案例。
2. 写下需要保留的行为、错误、取消和数据 invariant。
3. 先建立或迁移可失败的 Go test。
4. 用符合 Go 习惯的结构实现功能。
5. 必要时与固定版本的 TypeScript test oracle 比较。
6. 运行相关 unit、integration、race、fuzz 或 E2E test。
7. 更新 behavior ledger 和 test ledger。

不能仅凭“文件已翻译”或“测试数量接近”认定完成。

Behavior slice 必须关联一个主要领域模块和至少一个可观察行为，但只承担追踪作用。
同一模块里相互依赖的 behavior 应在一个里程碑内一起实现和联合审查；上游聚合文件中
互不相干的行为仍不能仅因同文件而混成同一职责。

测试通过是独立模块审查的输入，不等于审查结论。每个模块里程碑必须由未参与实现
的 reviewer 同时检查测试覆盖是否足以证明 contract、实现是否符合依赖和 ownership
约束，以及当前结构是否能自然接入后续迁移。审查结论和未关闭事项必须进入 ledger；
不能只保留在临时对话中。

## 模块 contract 与 workflow 验收

领域模块通过 contract test 固化职责和 invariant，完整产品通过跨模块 scenario 或
E2E workflow 验收。二者不能互相替代：

- module contract test 证明输入、输出、错误、取消、并发和 durable data 语义；
- workflow test 证明 provider、agent、tool、session 和 application 的组合顺序成立；
- public CLI/TUI test 证明用户可观察的输出、退出和 terminal lifecycle；
- differential test 只在行为仍不清楚时帮助建立证据，不定义产品架构。

首个 standalone workflow 至少贯穿 prompt、deterministic provider stream、agent
turn、一次 tool execution、assistant result、session 保存与恢复和 print mode
退出。后续 slice 应持续接入已有 workflow，或明确建立新的完整 workflow；不得
长期只积累相互未集成的 package-level unit test。

## 测试层次

### Unit test 与 property test

覆盖 message 转换、provider payload、配置、token/usage、path policy、错误映射、
文本宽度和 state invariant。Parser、版本化数据和输入组合优先采用 table-driven
test 与 fuzz。

### Component test

针对 provider、tool、storage、session 和 terminal component 建立可复用 test
suite。Component test 验证 pi-go 内部稳定语义，不要求为了测试而创建 public API
或 remote transport。

Component suite 的边界来自 module charter，而不是上游 TypeScript package。若
测试迫使 Go 代码暴露只为模拟上游内部 class 的接口，应重新检查测试是否关注了
实现结构而非产品行为。

### Agent 与 session scenario test

使用 deterministic fake provider、clock、ID、tool 和 filesystem，断言完整的
输出与 durable state。至少覆盖：

- 正常 streaming 与多轮对话；
- tool success、failure、timeout 和 malformed result；
- abort、retry、partial stream 和 provider error；
- session save、restart、resume、branch 和 compaction；
- cancellation 与并发完成顺序。

### CLI 与 TUI test

- CLI test 覆盖参数、环境变量、配置优先级、退出码、stdout/stderr 和 signal。
- TUI unit test 覆盖 layout、width、render、keybinding、editor 和 overlay。
- Terminal integration test 使用受控 pseudo-terminal，验证 resize、alternate screen、
  输入流和退出后的 terminal 恢复。
- Golden screen 更新必须逐项审查，不能用一次批量覆盖隐藏 rendering regression。

### Differential test

对于难以从源码安全概括的行为，隔离的 migration harness 可以把同一 fixture 分别
交给固定版本的 TypeScript pi 和 Go 实现，并在去除时间、随机 ID 等非确定字段后
比较结果。

Test oracle 不链接进 production binary，也不由产品代码启动。理解并稳定行为后，
优先用 Go test 和最小 golden fixture 固化，避免长期依赖双语言测试环境。

### 数据兼容与恢复测试

- 使用脱敏的历史 session 和配置 fixture 验证读取与升级。
- 对需要 round-trip 的 unknown data 验证不会丢失。
- 使用 fault injection 覆盖 partial write、rename failure、磁盘空间不足和进程中断。
- Corrupt data 必须产生明确错误，不能 panic 或 silent truncate。

### 真实 provider test

默认测试只使用 fake 或录制并脱敏的 fixture。真实 provider test 必须显式启用，
隔离 credential，并能按 provider 单独运行。外部服务波动不能阻塞默认 quality gate。

### E2E test

Core 阶段的 E2E test 直接启动 pi-go executable，覆盖 print mode 和 interactive
workflow，不经过为外部项目设计的 adapter。

Extension 和 pi-web test 只在对应后续阶段增加，并与 core test suite 分层。外部
集成失败不能被误报为 core 行为已经迁移，也不能通过启动 TypeScript pi 绕过。

## Go 特有的回归保护

- shared state、streaming、cancellation 和 cache path 使用 `go test -race`。
- parser、framing、session 数据和 provider payload 使用 native fuzz test。
- goroutine 生命周期测试检查取消后不会继续写状态或泄漏资源。
- 文件和 subprocess test 验证 descriptor、pipe、process group 与临时资源释放。
- benchmark 只用于有明确性能风险的路径，优化必须提供前后数据。

## Fixture 规则

- Fixture 必须确定、最小、可审查且不包含 secret。
- 历史 fixture 记录来源 commit、格式版本和脱敏方式。
- Vendor payload 只保留测试语义所需字段，但不能删除影响后续请求的 metadata。
- Golden 更新作为行为变更审查，不能机械接受。
- 可以生成的 fixture 应同时保留生成器或来源说明。

## 默认 quality gate

开始编写 Go 代码后，每次变更至少保持以下命令通过：

~~~sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
~~~

涉及 concurrency、session、streaming、cancellation 或 shared cache 时，运行相关
race test。涉及 parser 或持久化格式时，运行 fuzz seed、历史 fixture 和恢复测试。

一个 behavior 只有在所属模块里程碑的正常路径、错误路径、取消行为、ledger 和独立
review 都完成后，才可标记为 `ported`。
