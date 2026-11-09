# 上下文管理与历史读取

`pi-go session` 提供会话发现、原始历史读取、上下文用量、摘要压缩和短交接换窗。
这些能力复用现有 Agent、工具执行、队列、SessionManager 和存储；不创建第二个 Agent run，
也不把历史查询或上下文管理交给独立的摘要代理。

## 入口与会话选择

```sh
pi-go session --help
pi-go session list
pi-go session --id <session-id> history --help
pi-go session --id <session-id> context --help
```

所有会话级命令位于 `session` 下。`--id`、`--agent-dir`、`--file`、`--json` 是命令组参数，
放在 `history`、`context` 或 `list` 前。显式 `--id` 优先，否则使用 `PI_SESSION_ID`。

模型通过已有的 bash 工具调用正在运行的程序：

```sh
"$PI_EXECUTABLE" session context status
"$PI_EXECUTABLE" session history read --view dialogue
```

Terminal 的 run/TUI/RPC、Web Core 和 GUI 内嵌 Core 都装配 CLI 访问入口。GUI 程序同样支持
`pi-go-gui session ...`。远程 GUI、移动端的模型工具运行于所连接的 Core，使用 Core 的入口。
自行嵌入 Go 核心时，可使用 typed Application command/query API，或在 production assembly 中
配置 `AttachSession`；核心不依赖 CLI 传输。

模型默认只收到简短能力说明，具体参数按需读取帮助。不会增加常驻模型工具定义或强制的
历史恢复清单。没有启用 bash 的接入可以直接调用相同核心 API。

## 两种上下文处理方式

### 摘要压缩

```sh
"$PI_EXECUTABLE" session context compact
"$PI_EXECUTABLE" session context compact --instructions '优先保留未完成事项'
```

沿用原版摘要生成、近期消息保留、hook、事件和有序持久化流程。`--instructions` 是给摘要模型的
附加指令。自动压缩仍使用已有配置和溢出恢复策略；短交接不会替换默认的自动摘要兜底。

### 短交接换窗

```sh
"$PI_EXECUTABLE" session context new --handoff-stdin <<'HANDOFF'
已定位取消后的状态恢复问题，主要看了 session_runtime.go 和 agent_loop.go。
接下来补充时序测试，再修复状态恢复；具体讨论和工具输出可从历史读取。
HANDOFF
```

也可使用 `--handoff TEXT` 或 `--handoff-file PATH`，三种输入方式必须选一种。
交接只需要当前进度、主要文件或模块、后续方向；不要求详尽复述历史，也不要求固定模板。

该命令直接保存主模型已写好的交接，不再调用摘要模型。切换后的活动上下文包含当前系统配置、
最新短交接、随后交付的排队输入和本窗口的新消息，不自动挑选或携带旧对话。
历史读取结果作为普通工具结果进入新窗口，模型自主决定如何查询以及需要读多少。

换窗发生在同一 Session 内。窗口 ID 是检查点 entry ID，第一个窗口为 `initial`。
连续换窗只使用最新一份交接；更早的交接和完整原文仍能直接查询，不需要递归展开摘要链。

交接由主模型在既有历史前缀后生成，有机会利用 provider 已建立的缓存。
新窗口首次读取的历史是新请求内容，后续轮次才可能复用它；原文曾在旧窗口出现，不保证首次
读取自动命中缓存。缓存用量和成本应按 provider 实际报告计算。

## 提醒与用量

```sh
"$PI_EXECUTABLE" session --json context status
```

状态包括 Session/窗口 ID、估算 tokens、模型上下文窗口、响应预留、请求预算、提醒阈值，
以及最近一次上下文请求的状态。估算取“最近可用 provider usage 加消息尾部”和“活动消息加
系统指令、工具定义的本地估计”中的较大值；不是 provider 专用 tokenizer 的精确计量。
整个 Session 日志的累计大小不参与活动窗口估算。

启用自动压缩且装配了访问入口时，在请求预算的约 80% 提醒一次：模型可以正常收尾，或主动
提交交接/请求摘要。相同窗口、模型和预算下不每轮重复提醒。模型或预算改变后重新判断；
换窗后的活动消息不再包含旧窗口的 usage，避免立即重复压缩。提醒不会作为真实用户消息写入
对话视图，也不会在最终回答之后专门增加一次模型调用。

请求预算由模型窗口减去现有 `compaction.reserveTokens` 得到。原有自动摘要阈值与溢出恢复
继续负责兜底；提醒本身不保证任意 provider 请求一定不会超限。具体配置见[配置文档](configuration.md)。

## 原始历史与对话视图

```sh
"$PI_EXECUTABLE" session history windows
"$PI_EXECUTABLE" session history read --view dialogue
"$PI_EXECUTABLE" session history read --window initial --view dialogue
"$PI_EXECUTABLE" session history search '关键词' --view dialogue
"$PI_EXECUTABLE" session history read --view raw --entry <entry-id>
```

默认查询当前分支的全部窗口。`--window current` 选择当前窗口，`--window <entry-id>` 选择
指定窗口；`--leaf <entry-id>` 显式选择其他分支。`--entry` 可以重复，按原始顺序返回。
`--from`、`--to` 是包含端点的记录范围；`--limit` 限定完整记录数量，默认 0 表示全部。
超过数量时返回下一条记录 ID，以 `--from` 续读；不会截断单条消息。

对话视图是原始记录的确定性过滤，跨 provider 使用相同规则：

| 内容 | 对话视图 |
| --- | --- |
| 用户和助手可见 text | 完整保留，包括中间说明和最终回答 |
| 原始 phase | 有则保留，没有则不推断；也读取统一文本块保存的 signature phase |
| 图片、附件 | 保留类型、位置、可用名称/MIME 和 entry/block 引用 |
| thinking、工具参数、工具回执 | 排除，可通过 raw 显式读取 |
| 合成交接、压缩摘要、系统提醒 | 排除 |

保留文本块顺序与边界，不强行配成一问一答，不归纳任务状态或推断执行过程。
工具并行调用的参数、toolCallId、结果、顺序和终止状态保留在原始记录中。
没有工具回执只表示日志中没有对应记录，不能据此断言执行失败。

搜索默认不区分大小写，`--case-sensitive` 可改为区分。返回命中原始行、entry ID、窗口、角色、
文本块 index、行号及块内 UTF-8 字节偏移；以同一 entry ID 展开即可读取完整内容。
raw 搜索也能查工具、thinking 和状态字段；`block=-1` 表示在完整原始 JSON 中命中。
默认 dialogue 查询不会再次搜索历史检索工具的输出副本；显式 raw 查询忠实保留这些工具记录。

CLI 不另设字符预算、消息正文截断或自动摘要。超长输出使用现有 bash 的 2,000 行/50 KiB
统一限制、完整输出文件及提示；使用已有 read 工具按行续读。完整输出文件是临时读取材料，
Session 原始记录与稳定引用仍是长期依据。

读取未运行的会话：

```sh
pi-go session --agent-dir /path/to/agent --id <session-id> history read
pi-go session --file /path/to/session.jsonl history read --view dialogue
```

`--file` 只读，不打开 writer、不迁移或改写文件。历史读取使用已有 ReadIndex，索引可从原始
JSONL 重建，不是第二份权威历史。旧格式若需要迁移，先通过原有会话打开流程完成迁移。

## 请求与执行边界

运行进程拥有 AgentSession。CLI 子进程通过只监听 loopback 的会话专属控制连接提交 typed
Application 命令；连接能力通过每次 bash 调用的 `PI_SESSION_ENDPOINT`、`PI_SESSION_TOKEN`
注入，不修改全局进程环境，不向模型提示或命令输出展示令牌。
会话 ID 必须匹配控制端点；会话替换或关闭会同步关闭对应连接。

运行中的请求先返回 `accepted`，表示已排队，不表示已切换成功。当前工具批次全部结束并按
原有顺序落盘后，AgentSession 消费请求，SessionManager 先提交检查点，再更新 Agent 消息。
下一次 provider 请求使用重建后的上下文。steering/follow-up 沿用现有队列，在检查点之后交付。
CLI 不在调用工具内部等待切换完成，因而不会形成父 Agent 与 bash 子进程的互相等待。

空闲会话通过既有运行准入同步完成命令，返回 `completed`。外部终端可使用 `--id` 对未运行的
会话执行命令；已有其他进程持有 writer 时会被文件锁拒绝，不会另开第二个 writer。

`context status` 显示当前进程最近一次请求的 `accepted`、`completed`、`failed` 或 `cancelled`。
中止或失败的 run 若没有到达切换边界，待处理请求会取消，不带入之后的新任务。
落盘失败保留原活动上下文；存储层原有的 commit-unknown/poisoned 语义保持不变。
进程在接受请求之后、提交之前退出时，请求尚未持久化，不能视为成功。

交接可传 `--request-id <stable-id>`：同一分支上重试相同 ID 和内容会返回已有检查点，
包括进程重启后的重试；相同 ID 配不同内容被拒绝。请求状态本身是运行态，持久化依据是检查点。
普通摘要命令仍沿用原有摘要提交语义，不提供跨重启的请求 ID 重放。

## 存储与兼容性

短交接使用 v3 `custom` entry，`customType` 为 `pi-go.context-handoff.v1`，data 包含请求 ID、
交接文字、上一窗口 ID 和切换前估算 tokens。entry 的 parentId 指向切换前分支位置，entry ID
即新窗口 ID。它不伪装成已经执行过全量总结的 compaction entry。

SessionManager 的同一上下文投影算法识别摘要与交接边界。交接清空旧消息保留范围，摘要保留
原版的近期范围；交接之后再做摘要，只处理当前窗口和上一份交接，不重新遍历被换出的旧历史。
模型与 thinking 等分支配置仍从完整分支恢复。旧 entry ID、分支结构和原文不改变。

短交接和 CLI 历史视图是 pi-go 的显式扩展，原版 pi 没有这些命令。
原版或旧版本读者可保留 custom 数据，但不会应用 pi-go 的换窗边界；不能把旧读者的活动上下文
解释当作支持换窗。原有摘要压缩与上游 fixture 的兼容路径保持原状。

职责和依赖见[核心架构](ARCHITECTURE.md)，验证入口见[测试文档](testing.md)。

## 后续评估

保留普通摘要作为对照。后续真实场景评估需要分别统计交接生成与历史恢复的调用、缓存命中、
未命中输入、输出、耗时和费用，检查原文可达性、用户纠正、来源准确性与任务继续执行。
冻结历史的信息恢复不能替代完整任务回放，也不能证明自动选择上下文策略一定更优。
不以尽量减少查询轮次为优先目标，不增加任务专用的恢复模板或自动重要性筛选。

阈值升级、独立保存交接草稿、跨 Session 搜索以及无交接时直接换窗的策略仍未引入。
当前方案使用显式主模型交接，保留原摘要兜底。
