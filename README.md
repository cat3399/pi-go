# pi-go

pi-go 的目标是用 Go 忠实重写 [pi](https://github.com/cat3399/pi) 的 Agent Runtime。
目标不是实现一个思路相近的 agent，而是保留原版的能力、分层、状态所有权、事件时序和
可观察行为。

Go 可以采用符合语言习惯的类型、并发和资源管理方式，但不能以此为理由压平架构、删除
能力或让调用方补偿核心行为。产品运行时只使用 Go，不嵌入、代理或 fallback 到 TypeScript
版 pi。

## 兼容基线

当前产品行为以原版实际使用的调用链为基准：

`AgentSessionRuntime → AgentSession → Agent → runAgentLoop → SessionManager`

原版 `packages/agent` 中的新 `AgentHarness` 与这条生产链并存；在 coding-agent 完成迁移
之前，pi-go 继续以生产链及其测试为主要兼容目标，同时跟踪 Harness 中已经稳定的新契约。

完成度按以下证据判断：

1. 原版实际源码和测试；
2. TypeScript/Go 共用的行为 fixture；
3. pi-web 的真实调用需求；
4. pi-go 代码和测试。

## 当前状态

pi-go 已经贯通真实生产链：

`Runtime → AgentSession → Agent → AgentLoop → Provider/Tools`

AgentSession 已组合 SessionManager，AgentLoop、stateful Agent、JSONL session、retry、
compaction、会话树和 Runtime replacement 生命周期均有真实实现。普通生产入口使用真实
HTTP Provider 和本地工具，deterministic fake 仅用于测试。

项目仍未达到完整移植验收。当前主要缺口是 reload 中的动态 extension/tool runtime 重建、
独立 bash、provider/model/auth 组合、统一 command/state/event 边界，以及完整的
TypeScript/Go 跨实现验收。

`cmd/pi-go -p` 是一次性 headless 诊断入口，不代表长期 Runtime 或 pi-web 接入已经完成。

## 范围

TUI、主题、JS extension loader、插件管理和其他纯交互外围可以后置。与 Agent 架构相交的
hook、custom message/session entry、skills、prompt templates、resource trust 和动态工具
契约不能因此省略。

## 开发检查

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
go build ./...
git diff --check
```

真实 Provider 测试必须显式 opt-in；fake 或本地 HTTP fixture 的通过不能替代 live 验收。

## 文档

- [核心架构](docs/ARCHITECTURE.md)
- [当前状态](docs/STATUS.md)
- [后续计划](docs/ROADMAP.md)
