# pi-go 仓库协作规则

开始修改前，先阅读 README.md、docs/PROJECT.md、docs/ARCHITECTURE.md、
docs/ROADMAP.md、docs/TESTING.md 和 docs/UPSTREAM.md。

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
- interface 必须来自实际使用者的需要并保持窄小，不得为模拟 TypeScript class
  或想象中的 extension 创建空泛 abstraction。
- 在新的设计决策明确批准之前，不得嵌入 JavaScript runtime，也不得为了
  extension source compatibility 而扭曲核心设计。

## 迁移与测试纪律

- 每个迁移行为都必须能够追溯到 docs/UPSTREAM.md 固定的 pi commit 中的上游源码
  或测试。
- 每个相关上游测试都要分类为 ported、strengthened、deferred、
  intentionally-incompatible 或 not-applicable。不得用大范围 skip 隐藏缺口。
- 功能和对应测试必须在同一个 feature slice 中完成。不得根据翻译文件数或源码
  行数宣称完成。
- 默认测试使用确定性的 fake provider。真实 provider 测试必须显式启用，
  且不能成为默认测试套件的依赖。
- 一个行为完成前，必须运行 gofmt、go test ./...、go vet ./...，以及相关的
  race 或 fuzz 测试。
- 只有在独立的上游同步变更中，才能更新 docs/UPSTREAM.md；同时必须更新
  behavior ledger、test ledger 和受影响的 fixture。

## 工作区安全

- 保留用户无关的修改，绝不对这些修改使用 Git 回退命令。
- 永远不要删除文件。确实需要移出仓库时，移动到 /tmp 并明确说明。
