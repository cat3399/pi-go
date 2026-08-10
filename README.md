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

项目仍未达到完整移植验收。当前唯一主线差距是完整的 TypeScript/Go AgentSession/Runtime
跨实现 workflow 验收，以及由共同场景证明的行为偏差。Surface/Application 基础架构已经完成；
原版 RPC 辅助命令、Provider/Auth 覆盖面、完整插件宿主和 WebUI 功能不作为当前 Agent 主线阻塞。
Host 已有权威 state、跨 session replacement 的单一有序 event stream，并覆盖 pi-web 当前除扩展
UI 外的全部直接 Agent 命令；`prompt` 按原版 preflight 时点异步确认。

九个固定上游 workflow 已经逐字段通过：rich image/tool/multi-turn/reopen；混合 queue mode、
clear queue、abort 后续跑与最终 `agent_settled`；Provider Retry-After 后的 Agent 自动重试；手动
compaction；context overflow 自动压缩并 continue 恢复；以及运行中 model/thinking/active tools/
system prompt 切换、同一 run 下一 tool turn 刷新与 reload；以及 tree navigation、源分支保留、
Runtime fork replacement、父会话与双 JSONL reopen；以及含坏行/orphan 的 session 恢复、原位续写
与再次打开；以及 skill/template 请求展开、动态 `images.blockImages` 全上下文过滤、
`thinkingBudgets` 和 rich tool image 的 durable/provider 分层。下一组聚焦 reload 与并发控制组合。

`cmd/pi-go-rpc` 已提供长期 stdio JSONL Runtime，用于协议验证、自动化和跨实现验收；它不再是
WebUI 的长期产品内核路径。`cmd/pi-go -p` 仍是一次性 headless 诊断入口。

长期产品形态是一个 transport-neutral Application Host，以及按需编译的 TUI、WebUI 和未来
GUI surface。每个 surface 只拥有呈现状态，通过同一套 command/query/event 边界驱动权威
`Runtime → AgentSession → Agent → AgentLoop`。首个原生 WebUI 主链已经由可选的 `pi-go-web`
进程内承载，不经过 JSONL 子进程；开发态仅使用 Next Server 提供 HMR 和 `/api` 代理。

## JSONL Agent Host

```sh
go build -o /path/to/pi-go-rpc ./cmd/pi-go-rpc
/path/to/pi-go-rpc --cwd /path/to/project
```

每行输入一个带可选 `id` 的 JSON command；response 与原版 AgentSession event 逐行输出。
也可以用 `--session` 恢复现有 JSONL，会话、模型、工具、队列、retry、compaction 和 fork 等
行为仍由同一个 `Runtime → AgentSession → Agent` 调用链拥有，RPC 层不维护影子状态。

该入口是受支持的外部 transport 和测试工具，不是 WebUI 内部架构。产品 surface 应在各自
composition root 中直接装配 Application Host。

## 可选 Surface

- 默认核心构建不依赖 WebUI 或未来 GUI；
- `surface/web` 高内聚地拥有 React、HTTP/SSE、静态资源和 Web 测试；
- `surface/tui` 拥有终端输入、渲染和终端生命周期；
- `internal/application.Supervisor` 统一管理多会话 Host/Runtime 生命周期；
- WebUI 作为独立 `cmd/pi-go-web` 目标构建，静态前端只进入该二进制；
- TUI、WebUI、GUI 共享 Agent/Application 能力，不共享渲染抽象；
- 浏览器前端可以继续使用适合 DOM 的 TypeScript/React，生产服务端和 Agent 运行时只使用 Go。

首次安装前端依赖：

```sh
make web-setup
# 无 make 环境可直接使用：./scripts/webui.sh setup
```

日常开发使用双进程模式，不执行静态导出：

```sh
make web-dev WEB_ARGS='--cwd /path/to/project'
# 等价：./scripts/webui.sh dev --cwd /path/to/project
```

Next HMR 监听 `127.0.0.1:30141`，并把 `/api/*` 代理到 `127.0.0.1:30142` 的 API-only Go
进程。修改前端由 Next 立即 HMR；修改 Go 会先在后台重新构建，成功后只重启 API，构建失败时
保留上一版可用进程并等待下一次修改。

生产构建和运行彼此独立：

```sh
make web-build
make web-run WEB_ARGS='--cwd /path/to/project'
# 等价：./scripts/webui.sh build && ./scripts/webui.sh run --cwd /path/to/project
```

默认监听 `127.0.0.1:30141`。可使用 `--listen`、`--agent-dir` 和 `--docs-dir` 覆盖装配路径。
`build-webui.sh` 在本地生成被 Git 忽略的 `surface/web/_frontend/out`，再用 `pi_go_webui` build tag 把它嵌入
可选二进制。默认 `go build ./...`/`go test ./...` 不需要这些产物；最终二进制运行时也不需要
Next Server、Node 或 `pi-go-rpc` 子进程。

当前已真实支持 Agent chat/SSE、会话 list/restore/context/tree/state/rename、基础模型枚举/选择和
CWD 入口。files/Git/worktree、models/auth 配置、session export/delete/auto-name、插件和 skills
管理仍明确未实现，详见能力账本。

详见 [Surface 架构](docs/SURFACES.md) 与 [WebUI 能力状态](docs/WEBUI.md)。

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
make web-check
make web-build
git diff --check
```

真实 Provider 测试必须显式 opt-in；fake 或本地 HTTP fixture 的通过不能替代 live 验收。

## 文档

- [核心架构](docs/ARCHITECTURE.md)
- [当前状态](docs/STATUS.md)
- [后续计划](docs/ROADMAP.md)
- [Surface 架构](docs/SURFACES.md)
- [WebUI 能力账本](docs/WEBUI.md)
