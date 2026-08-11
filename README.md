# pi-go

pi-go 的目标是使用 Go 忠实重写 [pi](https://github.com/cat3399/pi) 的完整 Agent Runtime。
它不是一个只复刻交互思路的相似项目，也不会通过能力降级、静态替代或 TypeScript fallback
来绕开原版语义。

Go 实现可以采用符合语言习惯的类型、并发和资源管理方式，但必须保留原版的能力边界、状态
所有权、事件时序、持久化格式和可观察行为。

## 长期目标

- 完整对齐 pi 的 `AgentSessionRuntime → AgentSession → Agent → AgentLoop → SessionManager`
  生产语义，包括工具、队列、重试、压缩、取消、会话树、资源和扩展中立契约；
- 以 Go Agent Core 作为唯一权威运行时，不在 Surface 或 transport 中复制 Agent 状态和策略；
- 在同一核心之上提供 CLI、TUI、GUI 和 WebUI；
- CLI、TUI 和原生 GUI 默认通过进程内 typed Go API 接入；
- 浏览器 WebUI 通过版本化 HTTP command/query/snapshot 与一条全局 SSE 接入；
- stdin/stdout JSONL 只服务外部自动化、跨语言调用和协议测试；
- Surface 可以独立演进交互和视觉实现，但不能改变或补偿 Agent Core 的产品语义。

核心调用关系：

```text
CLI / TUI / GUI ───────────────┐
                               ├─ application.API
Browser WebUI ─ HTTP + SSE ────┘        │
                                        ▼
                             application.Service
                                        │
                                        ▼
                              ApplicationSession
                                        │
                                        ▼
                    Runtime → AgentSession → Agent → AgentLoop

External automation ─ JSONL protocol → ApplicationSession
```

更完整的状态所有权和 Surface 通信约定见
[核心架构](docs/ARCHITECTURE.md) 与 [Surface 架构](docs/SURFACES.md)。

## 环境要求

- Go 1.25 或更高版本；
- WebUI 开发和构建需要 Node.js 22.19 或更高版本；
- 只构建 Go Core、CLI 或 JSONL RPC 时不需要 Node.js。

## 构建

构建不含静态 WebUI 的统一命令：

```sh
mkdir -p bin
go build -o bin/pi-go ./cmd/pi-go
```

查看全部子命令：

```sh
./bin/pi-go --help
./bin/pi-go web --help
```

## 运行

执行一次命令行 Agent 请求：

```sh
./bin/pi-go run -p "hello"
./bin/pi-go run -p "hello" --provider <provider-id> --model <model-id>
```

启动 stdin/stdout JSONL 自动化接口：

```sh
./bin/pi-go rpc --cwd /path/to/project
./bin/pi-go rpc --cwd /path/to/project --session /path/to/session.jsonl
```

启动不含静态页面的 Web API，适合前端开发或独立调试：

```sh
./bin/pi-go web --api-only --listen 127.0.0.1:30142 --cwd /path/to/project
```

通过自定义主机名访问时显式放行对应 Host（可重复指定）：

```sh
./bin/pi-go web --listen 0.0.0.0:30141 --allowed-host pi.local --cwd /path/to/project
```

## WebUI 开发

首次安装依赖：

```sh
make web-setup
# 或：./scripts/webui.sh setup
```

启动 Next.js HMR 和自动重载的 Go API：

```sh
make web-dev WEB_ARGS='--cwd /path/to/project'
# 或：./scripts/webui.sh dev --cwd /path/to/project
```

默认前端地址为 `http://127.0.0.1:30141`，Go API 监听 `127.0.0.1:30142`，前端将
`/api/v1/*` 同源代理到 Go 进程。

## WebUI 生产构建与运行

```sh
make web-build
make web-run WEB_ARGS='--cwd /path/to/project'
```

等价脚本命令：

```sh
./scripts/webui.sh build
./scripts/webui.sh run --cwd /path/to/project
```

生产构建会导出静态前端，并使用 `pi_go_webui` build tag 将资源嵌入 `bin/pi-go`。运行时
不需要 Node.js 或 Next.js server。

## 开发检查

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
make web-check
git diff --check
```
