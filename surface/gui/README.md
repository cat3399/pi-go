# pi-go GUI

`surface/gui` 是独立构建的桌面产品，不是默认 `pi-go` surface，也不是只连接服务器的
GUI 壳。它在同一个 `pi-go-gui` 进程内创建完整的 `application.Service` 和 Agent Runtime，
前端通过 Wails IPC 使用它；设置中的远程模式改用与 WebUI 相同的 HTTP/SSE 协议。

## 模块边界

- `main.go`：桌面 composition root、窗口和静态资源；
- `bridge.go`：本地 Core 的 query/command/event IPC adapter；
- `frontend`：很薄的 Wails 宿主；
- `../ui`：各图形宿主共享的 Workbench；
- `../web`：远程 HTTP/SSE adapter 和密码认证。

本 module 通过 `replace github.com/cat3399/pi-go => ../..` 链接本地 Core。根 module 不反向
依赖本 module，因此默认构建不会链接 Wails、CGO 或 GUI 资源。

## 开发与构建

```sh
make setup SURFACE=gui
make check SURFACE=gui
make dev SURFACE=gui
make build SURFACE=gui
```

`make check SURFACE=gui` 使用 Wails server build tag 做无窗口检查。`make build SURFACE=gui`
生成根目录下的 `bin/pi-go-gui`。macOS 原生产物需要 Xcode Command Line Tools；
Windows 和 Linux 需要 Wails 对应的平台 WebView/编译工具链。

共享 Workbench 覆盖本地 Core 启动、会话管理、prompt 与流式事件、模型和思考等级、
Markdown、图片附件、工具过程与详情、本地/远程切换和远程密码登录。

## 源码复用

这里只接受可维护的源代码。OpenCodex 的编译后 renderer、加密/混淆 bundle 和提取产物
不进入本项目。具体复用范围见 `THIRD_PARTY_NOTICES.md`。
