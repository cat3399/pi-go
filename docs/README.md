# pi-go 使用文档

pi-go 通过同一套 Go Agent 核心提供命令行、TUI、Web 和桌面入口。移动端连接远程服务。

- [启动与日常使用](usage.md)
- [配置、资源目录与首次导入](configuration.md)
- [Provider 与认证](providers.md)
- [模型配置](models.md)
- [工具、技能与提示模板](tools.md)
- [版本、运行状态与本地源码](self-knowledge.md)
- [核心架构](ARCHITECTURE.md)
- [Surface 架构](SURFACES.md)
- [桌面应用](gui.md)
- [Android 移动端](mobile.md)
- [测试与兼容性验证](testing.md)
- [构建与发布](RELEASING.md)

源码按职责组织：[internal/app](../internal/app/) 装配产品服务，
[internal/application](../internal/application/) 提供应用 API，
[internal/runtime](../internal/runtime/) 管理 AgentSession 生命周期，
[internal/agent](../internal/agent/) 实现 AgentSession、Agent 与 AgentLoop，
[internal/session](../internal/session/) 管理会话，[internal/provider](../internal/provider/)
调用模型，[internal/tool](../internal/tool/) 执行内置工具。
