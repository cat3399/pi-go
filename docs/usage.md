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

## 更新程序

各个 surface 只更新自身的发布产物。Terminal/Web 使用 `pi-go update`，GUI 使用
`pi-go-gui update` 或“设置 → 关于 → 应用更新”，Android 使用同一设置入口。命令行示例：

~~~sh
pi-go --version
pi-go update --check
pi-go update
pi-go update --version 0.2.3
pi-go update --force
pi-go update --check --json
~~~

示例版本号需替换为实际发布版本。`--check` 展示当前版本、目标版本、surface、平台、实际安装路径
及更新状态，只读取发布元数据和校验和，不下载程序或修改安装。检查与执行共用同一套版本选择：
按当前构建的 surface、操作系统和架构选择具有发布产物与 SHA-256 记录的最高稳定版本。仓库最新
Release 如果只包含其他 surface，不会成为当前安装的目标。web 版即使运行 TUI，也仍更新为 web 版。

默认更新不会降级；`--version` 接受 `MAJOR.MINOR.PATCH` 或 `vMAJOR.MINOR.PATCH`，用于安装
指定稳定版本，也可明确降级。目标版本缺少对应资产时直接报错。`--force` 允许同版本重装，不绕过
校验，也不允许默认更新隐式降级。`--self` 是默认自身更新的显式写法。模型数据更新使用
[pi-go models update](models.md)，不属于程序自身更新。

普通源码构建先使用 `--check` 查询；切换为官方便携包时显式传入 `--version`，若版本相同再加
`--force`。`go install` 安装会提示通过原 Go 安装流程重建目标版本，保留原构建选项。安装目录需要可写；
更新器不会自动提权。GUI 的设置入口也支持指定版本与同版本重装；
更新始终作用于当前设备上的客户端，与正在连接的远程 Core 无关。

下载经 HTTPS 和发布的 `SHA256SUMS` 校验后，程序会核对新二进制的 Go 构建身份、surface、
平台及版本，再替换当前文件。GUI 的单个程序包含前端和内嵌 Core，两者一起更新；
旧版 GUI 的版本从二进制符号读取，新版也提供 `--version`。更新不会创建 Agent 会话、修改设置或重启服务。
成功后重新启动程序以使用新版本；正在运行的进程继续使用已加载版本。

旧程序保留在安装目录旁的 `.pi-go-update-*/previous-pi-go`（Windows 为
`previous-pi-go.exe`；GUI 为 `previous-pi-go-gui` 或 `previous-pi-go-gui.exe`），成功输出会给出准确路径。下载或验证失败不会修改原程序；Unix 原子替换
目标文件，Windows 的第二次移动失败会尝试恢复旧文件。若被强制终止或恢复失败，可从该目录
将旧程序恢复到原安装路径。下载文件保留在系统临时目录，失败的暂存目录也会保留以便检查。
校验和提供下载完整性校验；当前发布尚未对校验文件签名。

Android 在应用内检查匹配当前平台的最新版 APK，点击“下载 APK”后交给系统浏览器下载，
完成后手动打开 APK 安装。应用不申请安装权限、不自动覆盖自身，也不会把打开下载链接报告为
安装成功。源码 APK 同样可以下载最新版；实际手动覆盖安装仍由 Android 检查签名和版本。

`--json` 把检查计划或安装结果写入 stdout，进度及错误写入 stderr。检查结果包括
`current_version`、`target_version`、`surface`、`os`、`arch`、`executable`、
`update_available`、`can_update`、`action` 和可能的 `reason`；安装结果另外包含 `updated`
及旧程序的 `backup` 路径。退出码：`0` 表示检查或操作成功（包括无需更新），`1` 表示失败，
`2` 表示参数错误。网络错误不会被当作“已经最新”。
