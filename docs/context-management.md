# 上下文管理与历史读取

`pi-go session` 提供会话发现、原始历史读取、上下文用量和短交接换窗。
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

系统提示词不包含交接或历史检索说明。临近预算阈值时，运行时提醒携带交接命令；
换窗后的恢复消息才提供历史检索入口。不会增加常驻模型工具定义或强制的历史恢复清单。
没有启用 bash 的接入可以直接调用相同核心 API。

## 统一的短交接流程

### 手动请求交接

```sh
"$PI_EXECUTABLE" session context compact
"$PI_EXECUTABLE" session context compact --instructions '优先保留未完成事项'
```

界面压缩、`/compact` 和此命令都向当前 Agent 发送运行时交接要求；`--instructions`
是补充关注点。Agent 使用正常工具循环准备交接，再通过下面的 `context new` 命令提交。
普通文本回复不视为提交，不会由程序提取正文或自动换窗。

系统提示、活动消息、工具定义、模型、thinking、输出配置、认证和重试都沿用正常 Agent 请求；
交接没有单独的输出 token 上限，也不禁用工具。运行中收到手动要求时进入既有 steering 队列，
不中断当前工具批次；空闲时通过同一 Agent 执行路径启动交接，在提交完成后结束手动操作。

### 提交已经写好的交接

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

状态包括 Session/窗口 ID、估算 tokens、模型上下文窗口、有效交接阈值、提前提醒阈值，
以及最近一次上下文请求的状态。JSON 中 `requestBudget` 是有效交接阈值，`reminderTokens`
是提前提醒阈值，`reserveTokens` 是窗口减去有效预算得到的余量（最低为 0）。估算取
“最近可用 provider usage 加消息尾部”和“活动消息加系统指令、工具定义的本地估计”中的较大值；
不是 provider 专用 tokenizer 的精确计量。整个 Session 日志的累计大小不参与活动窗口估算。

所有提醒参数仍位于 `settings.json` 的 `compaction` 中，单位统一为 tokens，例如：

```json
{
  "compaction": {
    "enabled": true,
    "reminderAtTokens": 40000,
    "handoffAtTokens": 50000
  }
}
```

`enabled` 保持原含义：控制自动预算提醒，默认开启；手动请求不受它影响。
`handoffAtTokens` 直接指定升级交接提醒的用量，`reminderAtTokens` 直接指定提前提醒的用量。
显式阈值是正整数，提前提醒不能晚于交接提醒。新字段配置的数值不会随模型窗口大小自动缩放。

兼容读取只发生在配置装配和 reload 中，不运行迁移命令，也不改写原配置或历史会话：

- 配置了 `handoffAtTokens` 就使用它；未配置时沿用“模型窗口减去 `reserveTokens`”的旧算法。
  旧 `reserveTokens` 默认 16,384；预留大于等于窗口时仍按旧规则使用窗口的四分之一，显式 0 保持 0。
- 配置了 `reminderAtTokens` 就使用它；未配置时沿用有效交接预算约 80% 的默认提前提醒。
- `keepRecentTokens` 的读取和存储保持兼容，新交接流程仍不使用近期消息保留预算。

每次实际请求模型前检查活动上下文，包括新输入和工具结果。到达提前提醒点时给出剩余工作
额度和 CLI 交接命令：能完成就收尾，否则准备交接；到达交接阈值时升级为“现在通过 CLI 提交交接”。
相同窗口、模型和有效阈值下，每级只提醒一次；阈值在 reload 后改变时重新判断。
换窗后清除旧窗口的消息和 usage。提醒不作为用户对话落盘，不改变系统提示、工具或正常输出配置，
也不会在最终回答之后专门增加一次模型调用。

所有新窗口都必须由 `context new` 提交。模型未提交、调用失败或操作取消时，保留旧窗口；
provider 已返回上下文溢出时保留错误，不自动创建“无交接恢复”检查点或循环重试换窗。
可切换到容量足够的模型后重新请求交接，或显式通过 CLI 提交已有交接。
提醒和本地估计不保证任意 provider 请求一定不会超限。具体配置见[配置文档](configuration.md)。

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
已经落盘但尚未被模型处理的新输入由检查点引用保留，重启后也不会丢失。
所有触发方式沿用压缩开始/完成事件；界面显示“正在压缩上下文”，历史中显示可展开的
“上下文已压缩”记录，正文为 CLI 提交的短交接。
CLI 不在调用工具内部等待切换完成，因而不会形成父 Agent 与 bash 子进程的互相等待。

空闲会话通过既有运行准入同步执行请求；`compact` 等到模型通过 CLI 提交后返回 `completed`，
未提交则返回失败。运行中的 `compact` 只接受交接要求，随后 `new` 的提交完成才将该要求标为完成。
手动交接取消会中止正在生成或执行工具的 Agent 调用，并撤回尚未交付的交接要求；用户队列保持原有语义。
外部终端可使用 `--id` 对未运行的
会话执行命令；已有其他进程持有 writer 时会被文件锁拒绝，不会另开第二个 writer。

`context status` 显示当前进程最近一次请求的 `accepted`、`completed`、`failed` 或 `cancelled`。
中止或失败的 run 若没有到达切换边界，待处理请求会取消，不带入之后的新任务。
落盘失败保留原活动上下文；存储层原有的 commit-unknown/poisoned 语义保持不变。
进程在接受请求之后、提交之前退出时，请求尚未持久化，不能视为成功。

交接可传 `--request-id <stable-id>`：同一分支上重试相同 ID 和内容会返回已有检查点，
包括进程重启后的重试；相同 ID 配不同内容被拒绝。扩展 hook 改写交接正文时，检查点同时保留
原提交内容用于重放校验。请求状态本身是运行态，持久化依据是检查点。
手动请求模型生成交接的 `compact` 命令不提供跨重启的请求 ID 重放；需要安全重放时，
使用 `new` 提交固定交接与稳定 request ID。

## 存储与兼容性

短交接使用 v3 `custom` entry，`customType` 为 `pi-go.context-handoff.v1`，data 包含请求 ID、
交接文字、上一窗口 ID、切换前估算 tokens、未处理输入的 entry 引用及可选扩展元数据。
模型生成交接时的 usage 随普通 assistant entry 保存。旧版本的 `noHandoff` 检查点仍可读取。entry 的 parentId 指向切换前分支位置，entry ID
即新窗口 ID。它不伪装成已经执行过全量总结的 compaction entry。

SessionManager 的同一上下文投影算法识别历史摘要与交接边界。新交接不自动携带旧对话，
只保留未处理输入；已有原版 compaction 记录仍按原始范围读取，便于打开旧会话。
模型与 thinking 等分支配置仍从完整分支恢复。旧 entry ID、分支结构和原文不改变。

短交接和 CLI 历史视图是 pi-go 的显式扩展，原版 pi 没有这些命令。
原版或旧版本读者可保留 custom 数据，但不会应用 pi-go 的换窗边界；不能把旧读者的活动上下文
解释当作支持换窗。底层旧格式和分支摘要仍由上游 fixture 验证；AgentSession 的压缩策略
明确改为短交接，与原版自动全历史摘要不同。

职责和依赖见[核心架构](ARCHITECTURE.md)，验证入口见[测试文档](testing.md)。

## 后续评估

后续真实场景评估需要分别统计交接生成与历史恢复的调用、缓存命中、
未命中输入、输出、耗时和费用，检查原文可达性、用户纠正、来源准确性与任务继续执行。
冻结历史的信息恢复不能替代完整任务回放，也不能证明自动选择上下文策略一定更优。
不以尽量减少查询轮次为优先目标，不增加任务专用的恢复模板或自动重要性筛选。

独立保存交接草稿和跨 Session 搜索尚未引入。
