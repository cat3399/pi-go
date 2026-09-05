# pi-go GUI

桌面应用在 `pi-go-gui` 进程中运行完整的 `application.Service` 和 Agent Runtime，
Workbench 通过 Wails IPC 使用本地 Core。设置中的远程模式使用与 Web UI 相同的 HTTP/SSE 协议。

## 模块

下表路径相对于 `surface/gui/`：

| 路径 | 职责 |
|---|---|
| `main.go` | 桌面入口、窗口、静态资源和 Core 装配 |
| `bridge.go` | 本地 Core 的 query/command/event IPC |
| `frontend/` | Wails 前端宿主 |
| `../ui/` | 共享 Workbench |
| `../web/` | 远程 HTTP/SSE 和认证 |

本 Go module 通过 `replace github.com/cat3399/pi-go => ../..` 链接根模块。
GUI 的源码资料与 Core 一起内嵌，安装规则见[本地源码](self-knowledge.md)。

## 开发与构建

从仓库根目录执行：

```sh
make setup SURFACE=gui
make check SURFACE=gui
make dev SURFACE=gui
make build SURFACE=gui
```

检查使用 Wails `server` build tag，可以在无窗口环境运行。构建输出 `bin/pi-go-gui`。
macOS 需要 Xcode Command Line Tools；Windows 和 Linux 需要对应的编译工具链与 WebView 依赖。

Workbench 提供会话管理、流式对话、模型与思考等级选择、Markdown、图片附件、工具详情、文件预览、
本地/远程切换及远程密码登录。公共通信契约见 [Surface](SURFACES.md)，
源码归属与许可证见[共享 UI 第三方说明](../surface/ui/THIRD_PARTY_NOTICES.md)。
