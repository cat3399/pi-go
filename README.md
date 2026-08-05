# pi-go

pi-go 的目标是用 Go 完整重写 [pi](https://github.com/cat3399/pi) 的 Agent Runtime。
完成后的 Go package 应提供与原版 Pi package 等价的能力、分层、行为和数据结构；
pi-web 只需对进程启动与传输适配做少量调整，不需要迁就一个能力缩水的后端。

产品运行时只使用 Go，不嵌入、代理或 fallback 到 TypeScript 版 Pi。原版源码与测试是
兼容性基准；Go 可以采用更合适的类型、并发和资源管理方式，但不能因此删除能力、压扁
架构层次或改变可观察语义。

## 首期目标

当前首期只完成内部 Agent Runtime：

- 对齐原版 `AgentLoop`、`Agent`、`SessionManager`、`AgentSession` 与应用 Runtime 的职责；
- 对齐 Model、AgentMessage、ToolResult、session entry、event 与 hook 等核心契约；
- 把 retry、compaction、queue、动态配置、工具执行和持久化接入同一条产品调用链；
- 提供可由 Go 代码直接驱动的长期 runtime，并用端到端场景证明行为完整。

JSONL RPC、pi-web 接入和 TUI 都不属于当前里程碑。RPC 的实现可以后置，但它未来需要
暴露的状态、命令结果和事件语义必须在内部核心阶段形成，避免传输层反向塑造 Agent。
Provider 的数量也可以后置；完整 `Model` 结构和厂商无关的 Provider contract 不能后置。

这是快速重构期。仓库现有的内部 package、文件布局和旧文档不构成兼容性约束；如果它们
偏离原版架构，应直接重组或替换，而不是围绕旧实现做最小补丁。

## 当前状态

P1 AgentLoop 与 P2 stateful Agent 已收口：`AgentLoop` 是唯一的 Provider/Tool 执行核心；
`Agent` 只持有内存状态、监听器、队列和 active run，并以 AgentLoop 完成 prompt、continue、
streaming、工具链和 settlement。持久化、retry 与 compaction 位于 `AgentSession` 边界，不在
Agent 内形成第二套执行路径。

当前下一优先级是 P3 SessionManager 的完整产品语义，随后是 P4 AgentSession 的完整装配与
组合场景。内部 Runtime 尚未达到首期总验收条件。

现有 `cmd/pi-go -p` 是诊断入口，不代表产品 Runtime 已完成。更精确的实现盘点和已知测试
基线见 [当前状态](docs/STATUS.md)。

## 开发检查

```sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
```

涉及 agent、streaming、session、tool 并发或取消时，还应运行相关 race test。测试默认
使用 deterministic fake，不依赖真实 credential、网络或 TypeScript runtime。

## 文档

- [核心架构](docs/ARCHITECTURE.md)：目标分层、职责和兼容性边界。
- [实现路线](docs/ROADMAP.md)：内部核心优先的阶段与验收门槛。
- [当前状态](docs/STATUS.md)：当前实现事实、主要差距和验证基线。

这些文档只保留当前有效的开发契约，不维护历史迁移台账。完成度由代码、测试和完整行为
场景证明，不由文件数量或文档中的勾选项证明。
