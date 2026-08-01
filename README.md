# pi-go

pi-go 是 [pi](https://github.com/cat3399/pi) 的 Go 重写项目。首要目标是实现一个
能够独立运行、独立测试、独立发布的 Pi，而不是先把它建设成某个外部项目的
backend。

当前处于文档初始化阶段：仅建立项目目标、迁移边界和 Git 基线，尚未创建 Go
module 或业务源码。

## 项目目标

- 以 Go 为一等实现语言，逐步完整迁移 pi 的产品功能。
- 同步迁移上游测试，并根据 Go 的风险模型补充 race、fuzz、恢复和 E2E test。
- 允许早期版本功能较少，但每项已实现功能都必须是清晰、可维护的纯 Go 实现。
- 建立可持续的上游同步机制，使尚未迁移和有意不同的行为始终可见。
- 在 standalone pi-go 成熟后，再从真实需求中提炼稳定的 extension surface 和
  外部接入方式。

## 基本立场

- 产品 runtime 只使用 Go，不维护 TypeScript/Go 双实现和 runtime fallback。
- TypeScript 版 pi 只作为迁移参考、fixture 来源和隔离的 test oracle。
- 行为和数据兼容优先于源码结构兼容；不机械复制 TypeScript package 或 class。
- 初期不为 plugin、extension、pi-web 或其他外部使用者冻结 public API。
- pi-go 未来提供通用、可适配的能力；具体兼容逻辑由外部项目或 adapter 负责。
- transport、protocol 和进程边界必须由已经稳定的产品能力与实际需求决定，当前
  不预设 HTTP、SSE、CBOR 或其他方案。

整体顺序是：先完成 pi-go 产品本体，再提炼 extension 能力，最后处理 pi-web
等外部集成。完整目标和验收口径见 [docs/PROJECT.md](docs/PROJECT.md)。

## 文档

- [项目目标与约束](docs/PROJECT.md)
- [架构边界](docs/ARCHITECTURE.md)
- [迁移路线](docs/ROADMAP.md)
- [测试与回归策略](docs/TESTING.md)
- [上游基线](docs/UPSTREAM.md)
