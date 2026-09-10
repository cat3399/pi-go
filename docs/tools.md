# 工具、技能与提示模板

默认活动工具为 `read`、`bash`、`edit`、`write` 和 `terminal`，从首次模型请求起提供完整定义。
注册表同时提供 `grep`、`find`、`ls`。活动工具由当前会话控制；TUI 的 `/tools` 可调整配置，
系统提示随实际活动工具重建。

`read` 支持文本和图片，文本可用 `offset` / `limit` 分段读取；`write` 写入完整文件；
`edit` 对匹配的文本进行替换；`bash` 执行工作目录中的命令，并报告截断信息和完整输出文件。
准确参数以当前工具 schema 为准，实现见[工具定义](../internal/tool/registry.go)。

查阅产品文档时，从系统提示提供的 README 和文档目录开始。工具的相对路径以会话工作目录
为基准；文档中的相对链接以该文档所在目录为基准，传给工具时使用解析后的绝对路径。
当前模型和会话信息见[运行状态](self-knowledge.md)。

## 持久终端

`terminal` 是默认内置工具，模型可直接创建和操作终端，调用过程中不改变工具集合或系统提示。
Workbench 标题栏的终端按钮打开右侧面板，可新建、
切换和输入；工具调用卡片也能打开对应终端。面板只负责终端交互，不修改模型的工具配置。
关闭面板只断开视图，停止按钮才结束进程。TUI 保持普通工具结果展示，不提供终端面板。
原有 `bash` 和 `! / !!` 行为不变。

Agent 整轮运行期间（包括工具执行、重试等待和压缩），用户终端只读：可查看、切换、滚动和复制，
不能输入、新建、中断或停止终端。此限制由 AgentSession 执行，适用于所有 Surface；Agent 自身的
终端工具调用不受影响。运行结束后恢复用户操作，已启动的进程继续运行。

`action: "open"` 是唯一的新建操作，创建真实交互 shell 并返回 ID；`list` 列出当前会话的终端。
`run`、`read`、`write`、`interrupt`、`close` 必须提供已有终端的非空 `id`；缺失、空白或未知 ID
都会报错，不会创建终端。`cd`、环境变量、交互程序及后台任务保留在对应 shell 中，不同终端相互独立。
`run` 发送命令文本并补换行，支持多行命令；`write` 发送原始输入，`interrupt` 发送 Ctrl-C，
`close` 结束终端。`cwd` 和 `title` 用于 `open`，后续调用通过 ID 选择终端。

```json
{"action":"open","title":"开发终端","wait_ms":0}
{"action":"run","id":"<返回的 ID>","command":"export PROJECT=demo; pwd","wait_ms":1000}
{"action":"run","id":"<返回的 ID>","command":"npm run dev","wait_ms":0}
{"action":"read","id":"<返回的 ID>","mode":"follow","wait_ms":1000}
```

每次执行只读取发送输入之后的输出，默认 `read` 从模型自己的游标继续，不重复历史。
`since` 读取增量，`tail` 查看最近输出，`follow` 等待下一段输出；可用 `after` 指定字节游标。
各 Surface 独立读取，不会消费模型或其他视图的输出。`include_user: true` 可查看用户活动；
默认按最近一次输入来源省略用户区间。这是普通终端流的近似归属，并发后台输出可能交叠，
不使用命令后缀、提示符解析或 shell hook 来制造精确命令边界。

`wait_ms` 默认 1000，设为 0 立即返回，单次操作硬上限为 30 秒；等待结束不会杀死命令，
`idle` 也不表示命令执行完成。`max_bytes` / `max_lines` 可降低每次返回的上限，硬上限为
50 KiB / 2000 行。进程退出码仅表示整个 shell 退出，不表示单条命令的状态。

原始输出写入权限受限的临时日志，截断结果包含日志路径。每个终端的内存回放保留最近
1 MiB；日志达到 32 MiB 或写入失败时停止该终端并报告原因。单次输入最多 64 KiB，
每个会话最多 16 个活动终端、32 个可重开的终端记录。淘汰记录不会删除已经返回的日志。
重新打开视图回放保留区间，超过区间的早期屏幕状态不会恢复。

终端服务归 AgentSession 所有，工具 reload 和浏览器断连不影响进程；会话关闭或运行时替换
会结束终端。活跃终端阻止空闲会话回收，但不会让 Agent 显示为正在生成。终端进程不会跨
应用重启恢复，也不复制到 fork 会话中。

## 技能

用户技能位于用户数据目录的 `skills/`；项目技能位于 `.pi-go/skills/`。共享
`~/.agents/skills/` 和项目祖先目录中的 `.agents/skills/` 也参与发现。项目资源遵循信任设置。

每个技能目录包含 `SKILL.md`：

```markdown
---
name: review
description: Review the project's code and explain actionable findings.
---
Read the relevant code and tests before producing the review.
```

系统提示列出可用技能的名称、说明和文件位置，Agent 按需读取正文。也可使用
`/skill:review <任务>` 显式调用。`disable-model-invocation: true` 将技能从自动发现提示中隐藏，
显式调用仍可使用。技能内的相对引用以技能目录为基准。

## 提示模板

用户模板位于用户数据目录的 `prompts/`；项目模板位于 `.pi-go/prompts/`。例如
`prompts/review.md`：

```markdown
---
description: Review a specific component
---
Review $1, with attention to $ARGUMENTS.
```

使用 `/review parser` 展开模板。模板支持位置参数、默认值和参数切片，完整语法实现见
[模板展开实现](../internal/resource/prompt.go)。资源配置、信任和 `/reload` 见 [配置](configuration.md)。

## 扩展契约

Go 核心提供工具结果、事件、自定义消息、生命周期 hook 和资源来源等 typed 契约，供进程内
装配使用。TypeScript 扩展 / npm 包的发现和执行不在当前生产加载器的能力范围内。
接口定义见[扩展契约](../internal/agent/extensions.go)，状态所有权见 [核心架构](ARCHITECTURE.md)。
