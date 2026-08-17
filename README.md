# pi-go

pi-go 是 [pi](https://github.com/cat3399/pi) Agent Runtime 的 Go 实现。项目以同一套 Go Core
提供命令行、终端、Web、桌面和移动端入口，并保持会话、工具、模型调用和事件语义一致。

## Surface

| Surface | 内容 | 产物 |
|---|---|---|
| `terminal` | CLI、TUI、RPC | `bin/pi-go` |
| `web` | CLI、TUI、RPC、内嵌 Web UI | `bin/pi-go` |
| `gui` | 内嵌完整 Core 的桌面应用 | `bin/pi-go-gui` |
| `mobile` | 连接远程 Core 的 Android 应用 | `bin/pi-go-mobile.apk` |

## 构建

所有 Surface 使用同一组 Make 命令：

```sh
make help
make setup SURFACE=<surface>
make check SURFACE=<surface>
make build SURFACE=<surface>
make dev SURFACE=<surface>
make run SURFACE=<surface> ARGS='...'
```

`SURFACE` 默认为 `terminal`：

```sh
make build
make build SURFACE=web
make build SURFACE=gui
make build SURFACE=mobile
```

交叉编译带 Web UI 的 Linux AMD64 版本：

```sh
make setup SURFACE=web
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build SURFACE=web
```

移动端工具链和设备：

```sh
make doctor SURFACE=mobile
make devices SURFACE=mobile
```

## 运行

```sh
./bin/pi-go run -p "hello"
./bin/pi-go tui
./bin/pi-go rpc --cwd /path/to/project
```

启动 Web 服务：

```sh
PI_GO_WEB_PASSWORD='change-me' ./bin/pi-go web \
  --listen 0.0.0.0:30141 \
  --cwd /path/to/project
```

非 loopback 地址必须设置 `--password` 或 `PI_GO_WEB_PASSWORD`。

## 文档

- [核心架构](docs/ARCHITECTURE.md)
- [Surface 架构](docs/SURFACES.md)
