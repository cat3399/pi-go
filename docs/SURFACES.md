# Surface 架构

终端、Web 和桌面入口运行同一套 Go Core；Android 连接远程 Core。
GUI、Web 和移动端共用 `surface/ui` 的 React Workbench。

## 入口与通信

| 入口 | 通信方式 | Core 所在位置 |
|---|---|---|
| CLI `run` | 同进程 Runtime 调用 | 本机进程 |
| TUI | 同进程 typed Application API | 本机进程 |
| 桌面 GUI 本地模式 | Wails IPC → Application API | 桌面进程 |
| 桌面 GUI 远程模式 | HTTP/SSE | 远程 Web 服务 |
| 浏览器 Web UI | 当前 origin 的 HTTP/SSE | Web 服务 |
| Android | Go 网络桥接的 HTTP/SSE | 远程 Web 服务 |
| JSONL RPC | stdin/stdout → ApplicationSession | RPC 进程 |

Workbench 的 `ApplicationClient` 提供本地 IPC 和远程 HTTP/SSE 两种实现。
宿主负责窗口、导航、输入与连接适配；共享 UI 管理会话展示、项目选择、工具详情、文件预览和草稿。
状态所有权见[核心架构](ARCHITECTURE.md)。

## Application API 与 Web 协议

`internal/application.API` 提供 command、query、snapshot、项目目录、会话生命周期和应用级事件。
`application.Service` 管理多个独立的 ApplicationSession；各会话拥有自己的 Runtime。
`internal/protocol/v1` 为实际的协议消费者提供字段映射。

`surface/web` 的 HTTP 边界位于 `/api/v1`，主要端点包括：

| 方法和路径 | 用途 |
|---|---|
| `POST /api/v1/sessions` | 创建会话 |
| `GET /api/v1/snapshot` | 应用快照 |
| `GET /api/v1/sessions/{id}` | 单会话 durable/live 快照 |
| `POST /api/v1/sessions/{id}/commands` | 会话命令 |
| `POST /api/v1/projects` | 添加已有项目目录 |
| `DELETE /api/v1/projects` | 从项目列表移除目录 |
| `GET /api/v1/events` | 全局 SSE |

客户端共用一条 SSE 连接，按 `sessionId` 分发事件。SSE 使用 `id: <revision>` 与
`Last-Event-ID`；历史游标过期时发送 `reset_required`，客户端读取新 snapshot 后继续订阅。
慢客户端断开自己的订阅，Agent 执行不等待网络消费者。

Go 服务直接提供 HTTP，HTTPS 可由前置代理终止。桌面和移动端的远程连接支持 HTTP 与 HTTPS。

## 模块与装配

根 Go module 的 `cmd/pi-go` 装配 CLI、TUI、RPC 和 Web 服务。浏览器宿主位于
`surface/web/_frontend`：开发时 Next HMR 代理自动重载的 Go API，生产构建将静态资源
内嵌到 Go 二进制，运行时不依赖 Node。

`surface/gui` 是独立 Go module 和桌面 composition root，链接 Go Core、Wails bridge 与
Workbench 静态资源。根模块的检查与构建不遍历 GUI 模块。

`surface/mobile` 是独立 Go module，宿主包含 Android WebView 和 Go 网络桥接，不链接 Agent Core。

统一命令见[构建与发布](RELEASING.md)，平台工具链与开发步骤见
[Web](../surface/web/_frontend/README.md)、[GUI](gui.md)和[Android](mobile.md)。

## 第三方源码

共享 UI 的源码归属和许可证见[第三方说明](../surface/ui/THIRD_PARTY_NOTICES.md)。
各宿主模块的附加说明位于其目录中的 `THIRD_PARTY_NOTICES.md`。
