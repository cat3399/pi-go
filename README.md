# pi-go

pi-go 是 [pi](https://github.com/cat3399/pi) Agent Runtime 的 Go 实现，提供命令行、终端、
Web、桌面和 Android 入口。Go Core 管理模型调用、工具执行、会话树、队列、重试与上下文压缩。

用户数据存放在 `~/.pi-go/agent`，项目配置使用 `.pi-go`。首次使用时可以从已有的 pi 目录
自动复制兼容数据。包含 Core 的二进制随构建携带使用文档和源码，部署后仍可在本地查阅实现。

## 快速开始

在仓库根目录构建并运行终端版本：

```sh
make build
./bin/pi-go --help
./bin/pi-go tui
```

运行前通过环境变量或 `~/.pi-go/agent/auth.json` 配置 Provider 凭据，模型由设置或启动参数选择。
详细步骤见[使用说明](docs/usage.md)、[Provider 配置](docs/providers.md)和[模型配置](docs/models.md)。

Terminal/Web 使用 `pi-go update`，GUI 使用 `pi-go-gui update` 或设置页替换更新，
Android 在设置页下载最新版 APK。各入口选择自己的产物，详见[更新程序](docs/usage.md#更新程序)。

单次运行和自动化入口：

```sh
./bin/pi-go run --model openai/gpt-4.1 -p "介绍这个项目"
./bin/pi-go rpc --cwd /path/to/project
```

构建并启动 Web：

```sh
make setup SURFACE=web
make build SURFACE=web
PI_GO_WEB_PASSWORD='change-me' ./bin/pi-go web \
  --listen 0.0.0.0:30141 --cwd /path/to/project
```

非 loopback 监听必须设置 `--password` 或 `PI_GO_WEB_PASSWORD`。

## 产品入口

| Surface | 内容 | 本地构建产物 |
|---|---|---|
| `terminal`（默认） | CLI、TUI、RPC、Web API | `bin/pi-go` |
| `web` | 终端版本及内嵌 Web UI | `bin/pi-go` |
| `gui` | 内嵌 Go Core 的桌面应用，也可连接远程服务 | `bin/pi-go-gui` |
| `mobile` | 连接远程 Core 的 Android 应用 | `surface/mobile/bin/pi-go-mobile.apk` |

Web 开发使用 Next HMR 与自动重载的 Go API；生产运行只需内嵌静态资源的 Go 二进制。
统一命令和平台工具链见[构建与发布](docs/RELEASING.md)，通信方式与状态边界见
[Surface 架构](docs/SURFACES.md)。

## 文档

[文档索引](docs/README.md)提供全部使用与维护文档。

- [配置、目录与首次导入](docs/configuration.md)
- [工具、技能和模板](docs/tools.md)
- [版本、运行状态与本地源码](docs/self-knowledge.md)
- [核心架构](docs/ARCHITECTURE.md)
- [测试与兼容性验证](docs/testing.md)
- [构建与发布](docs/RELEASING.md)
