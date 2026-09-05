# Surface

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

非 loopback 监听必须配置 `--password` 或 `PI_GO_WEB_PASSWORD`；loopback 可选择无密码。
Go 服务直接提供 HTTP，HTTPS 可由前置代理终止。桌面和移动端的远程连接支持 HTTP 与 HTTPS。

## 构建命令

所有命令从仓库根目录执行，`SURFACE` 默认为 `terminal`：

```sh
make help
make setup SURFACE=<surface>
make check SURFACE=<surface>
make build SURFACE=<surface>
make dev SURFACE=<surface>
make run SURFACE=<surface> ARGS='...'
```

Terminal、Web 和 GUI 的 `make run` 使用已有产物；Mobile 的设备运行会先构建再安装。
版本和输出目录可以统一指定：

```sh
make build SURFACE=web VERSION=1.2.3 OUTPUT_DIR=/tmp/pi-go-build
```

### Terminal

根 Go module 构建 `cmd/pi-go`，输出 `bin/pi-go`：

```sh
make build
./bin/pi-go run -p "hello"
./bin/pi-go tui
./bin/pi-go rpc --cwd /path/to/project
./bin/pi-go web --api-only --cwd /path/to/project
```

该构建包含 CLI、TUI、RPC 和 Web API，不需要 Node、Wails 或平台 GUI SDK。

### Web

```sh
make setup SURFACE=web
make dev SURFACE=web ARGS='--cwd /path/to/project'
make check SURFACE=web
make build SURFACE=web
make run SURFACE=web ARGS='--cwd /path/to/project'
```

开发时 Next HMR 代理 `/api/v1/*` 到自动重载的 `pi-go web --api-only`。
生产构建通过 `pi_go_webui` tag 内嵌静态 export，运行时只需 Go 二进制。
浏览器宿主位于 [surface/web/_frontend](../surface/web/_frontend/README.md)。

Linux AMD64 交叉编译示例：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build SURFACE=web
```

### GUI

`surface/gui` 是独立 Go module 和桌面 composition root，链接 Go Core、Wails bridge 与
Workbench 静态资源，输出 `bin/pi-go-gui`。根模块的检查与构建不遍历 GUI 模块。
平台依赖和本地开发见 [GUI 文档](gui.md)。

### Mobile

`surface/mobile` 是独立 Go module，提供 Android arm64 APK，最低 Android 8.0 / API 26。
宿主包含 WebView 和 Go 网络桥接，不链接 Agent Core。产物为
`surface/mobile/bin/pi-go-mobile.apk`。设备、SDK 和签名说明见
[移动端文档](mobile.md)。仓库没有 iOS 构建入口。

## 第三方源码

共享 UI 的源码归属和许可证见[第三方说明](../surface/ui/THIRD_PARTY_NOTICES.md)。
各宿主模块的附加说明位于其目录中的 `THIRD_PARTY_NOTICES.md`。
