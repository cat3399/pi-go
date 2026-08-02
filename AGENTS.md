# pi-go 仓库协作规则

开始修改前，先阅读 README.md、docs/PROJECT.md、docs/ARCHITECTURE.md、
docs/SOURCE_MAP.md、docs/ROADMAP.md、docs/TESTING.md、docs/UPSTREAM.md 和
docs/migration/README.md。实现或审查模块里程碑时，还必须读取对应 module charter、
behavior/test ledger、workflow 和数据清单。

## 架构硬约束

- Go 是一等实现语言。生产 runtime 只能使用 Go，产品代码不得启动、导入、proxy
  到 TypeScript 版 pi，也不得在失败时 runtime fallback 到它。
- 首要目标是独立可用的 pi-go 产品。先迁移 core、CLI 和 TUI，再考虑 extension
  与 pi-web 等外部集成。
- 固定版本的 TypeScript 实现只能由隔离的 migration test 或 differential test
  调用，不能成为默认 test suite 和发布产物的依赖。
- 初期实现默认保持 internal。没有真实、稳定的使用需求时，不得提前冻结 public
  API、extension protocol 或 transport。
- 不得增加只对 pi-web 或某个 plugin 有意义的 route、DTO、字段、状态分支或
  package。具体兼容逻辑由外部项目或独立 adapter 负责。
- 当前不预设 HTTP、SSE、WebSocket、CBOR 等接入方案。迁移上游 protocol 时，
  根据其产品行为和数据兼容需要重新决策。
- 持久化数据必须支持向前兼容。不能仅因当前版本无法解释 unknown field 或
  event，就在读取和写回时破坏它们。
- 除非 TypeScript 的 package 或 class 边界同时也是合理的 Go domain boundary，
  否则不要照搬其结构。
- 领域模块里程碑是实现和独立 review 单位，behavior slice 是需求与测试追踪单位，
  完整用户 workflow 是跨模块验收单位。不得把上游 package、目录或热点文件直接
  当成迁移任务。
- 实现领域模块前，必须明确职责、依赖、state ownership、关键 invariant、上游
  证据、首批 behavior slice 和验收 workflow；module charter 不等于 public API。
- interface 必须来自实际使用者的需要并保持窄小，不得为模拟 TypeScript class
  或想象中的 extension 创建空泛 abstraction。
- 在新的设计决策明确批准之前，不得嵌入 JavaScript runtime，也不得为了
  extension source compatibility 而扭曲核心设计。

## 迁移与测试纪律

- 每个迁移行为都必须能够追溯到 docs/UPSTREAM.md 固定的 pi commit 中的上游源码
  或测试。
- 每个相关上游测试都要分类为 ported、strengthened、deferred、
  intentionally-incompatible 或 not-applicable。不得用大范围 skip 隐藏缺口。
- 一个模块里程碑内的相关行为和测试应一起实现；小组件只做过程自检，不逐项停下来
  做独立 review。不得根据翻译文件数或源码行数宣称完成。
- 调研热点文件时必须沿调用者、数据和行为边界读取相关证据。不得把
  `interactive-mode.ts`、`agent-session.ts` 等聚合文件作为一次整体翻译任务。
- 默认测试使用确定性的 fake provider。真实 provider 测试必须显式启用，
  且不能成为默认测试套件的依赖。
- 一个模块里程碑完成前，必须运行 gofmt、go test ./...、go vet ./...，以及相关的
  race 或 fuzz 测试。
- 每个领域模块里程碑在标记完成、或允许依赖它的下一模块扩大实现范围前，必须由
  未参与该里程碑实现的 reviewer 独立审查。审查至少覆盖上游行为证据、规则与
  module charter 符合性、实现正确性、测试缺口、依赖方向、state ownership、
  并发与数据安全，以及当前设计能否继续承载后续迁移。
- 独立审查发现的 blocker 必须先关闭；允许延期的问题必须写入 ledger，明确影响、
  owner module 和重新评估条件。严重阻塞不得静默降级或以兼容分支绕过。
- 只有在独立的上游同步变更中，才能更新 docs/UPSTREAM.md；同时必须更新
  behavior ledger、test ledger 和受影响的 fixture。

## 工作区安全

- 保留用户无关的修改，绝不对这些修改使用 Git 回退命令。
- 永远不要删除文件。确实需要移出仓库时，移动到 /tmp 并明确说明。
- 委派子 agent 时，任务说明必须明确禁止其继续派生子 agent。
