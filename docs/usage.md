# 启动与日常使用

```sh
pi-go --version
pi-go --help
pi-go run --model openai/gpt-4.1 -p "检查当前项目的构建配置"
pi-go tui --cwd /path/to/project
pi-go rpc --cwd /path/to/project
```

模型需要有可用的[认证](providers.md)。`run` 要求显式提供 `-p`，结束后退出；`tui` 提供
持续交互；`rpc` 提供 JSONL 命令与事件。CLI 用 `--session /path/to/session.jsonl` 打开或建立
指定会话。TUI 用 `--session <session-id>` 打开已有会话，也可以使用 `/resume`。

Web 入口需要带 Web UI 的构建：

```sh
make build SURFACE=web
PI_GO_WEB_PASSWORD='your-password' ./bin/pi-go web --listen 0.0.0.0:30141 --cwd /path/to/project
```

绑定到非 loopback 地址时要求密码。开发时使用 `make dev SURFACE=web`，Next HMR 代理 API-only
Go 服务；生产程序内嵌静态 UI，运行时不需要 Node。GUI 可以运行本机核心或连接远程核心，
移动端连接远程核心。参见 [Surface 架构](SURFACES.md)。

TUI 常用命令：

| 命令 | 用途 |
|---|---|
| `/model`、`/thinking` | 选择模型与推理级别 |
| `/login`、`/logout` | 管理 Provider 凭据 |
| `/new`、`/resume` | 新建或打开会话 |
| `/tree`、`/fork`、`/clone` | 浏览和派生会话 |
| `/compact` | 压缩当前上下文 |
| `/tools` | 选择活动工具 |
| `/trust` | 信任当前项目的本地资源 |
| `/reload` | 重新加载资源和动态配置 |
| `/settings`、`/stats` | 修改设置、查看统计 |
| `/export`、`/import` | 导出或导入会话 |
| `/abort`、`/clear-queue` | 终止操作、清空等待消息 |

通过 `/help` 查看当前界面的完整命令和快捷键。会话以 JSONL 保存，包含消息、工具结果、模型
切换、分支和压缩等记录。队列、重试、压缩和持久化由核心统一执行。
