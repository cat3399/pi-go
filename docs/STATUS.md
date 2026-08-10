# 当前状态

本文只描述当前代码事实，不记录历史迁移过程。

## 总体判断

pi-go 已经不是原型。真实生产路径为：

`Runtime → AgentSession → Agent → AgentLoop → Provider/Tools`

AgentSession 已组合 SessionManager，生产入口没有使用 scripted/fake Provider 冒充真实模型。
AgentLoop、stateful Agent 和当前生产版 SessionManager 的主体完成度较高；AgentSession 与
Runtime 已实现大量产品能力，但尚未达到与原版等价的整体验收。

Surface/Application 基础架构已经完成，当前停止扩展 WebUI 功能，单线聚焦产品级 AgentSession
与 Runtime 的 TypeScript/Go 跨实现验收。静态 pi-web 前端由 `surface/web` 与 `cmd/pi-go-web`
承载，HTTP/SSE 直接调用进程内 `internal/application.Supervisor`/Host/Runtime；Surface 不维护
Agent 或 durable session 的第二套状态。

## 已实现

### AgentLoop 与 Agent

- text/thinking/tool streaming 和 multi-turn tool loop；
- 并行/顺序工具调度、rich ToolResult、usage/cost 和错误结果；
- dynamic per-turn snapshot、prepare-next-turn 和 graceful stop；
- AgentState、prompt/continue、listener、abort/wait；
- steering/follow-up 队列和 delivery mode。

### Session 与 AgentSession

- JSONL v3、typed entry、tree/context、branch/fork 和恢复；
- compaction、branch summary、model/thinking change、name/label/custom data；
- AgentSession 持久化、retry、Retry-After、overflow recovery；
- manual/auto compaction、tree navigation、model/thinking 控制；
- stats、context usage、last assistant text 和 runtime policy controls；
- 完整 built-in tool registry 与独立 active tool set，默认启用 read/bash/edit/write；
- active tools、provider-visible schemas 和 system prompt 的原子联动更新；
- extension command → input hook → `/skill:*`/prompt template 的统一 preflight 顺序；
- input handler 的有序 transform/handled/error isolation，以及流式 prompt 的 steer/follow-up 投递；
- custom message 的空闲持久化、`triggerTurn`、运行中 steer/follow-up 和 `deliverAs: nextTurn` 调度；
- 展开或 transform 后的真实输入进入 before-agent hook、Provider 和持久化会话。
- standalone bash 已接入真实 shell backend，支持 sanitized 流式更新、并发执行/全量 abort、
  `!`/`!!` context 可见性、截断产物和有序 `BashExecution` 持久化；shutdown 会先取消并等待执行落盘。
- 原位 reload 已按 `session_shutdown → settings → queue/runtime settings → resources →
  active tools/system prompt → rebind → session_start` 顺序实现，并保留运行中请求的 turn snapshot。

### Runtime、Provider、工具和服务

- create/switch/new/fork/import/dispose replacement 生命周期，以及不替换 AgentSession 的 Runtime reload；
- OpenAI Responses 与 Chat Completions 的真实 HTTP adapter；
- bash/read/write/edit/grep/find/ls 七个真实本地工具；
- settings、auth、model、resource、prompt、skills 和 trust 基础服务；
- deterministic fake、fault injection 和本地 HTTP fixture 测试设施。

## 尚未对齐

### Agent 状态与事件验收

- Agent 已是 model/thinking/system prompt/active tools/messages 的运行时唯一事实源；AgentSession
  只保留完整工具目录和构建 system prompt 所需的产品元数据。
- `agent_settled` 回调观察到的是 idle session，并允许立即开始下一次 prompt。
- 已建立固定上游 commit 的 `multi_turn_rich_tool_reopen` TypeScript/Go workflow oracle，生产
  `CreateAgentSession` 路径逐字段对齐 3 次 Provider 输入、51 个 AgentSession 事件、tool result、
  最终 state/stats、8 条 JSONL entry 及 reopen context。
- 该场景发现并移除了上游协议不存在的 `_piGoRawArguments` 持久化/RPC 字段；工具参数恢复现在
  以原版 `arguments` JSON 值为唯一事实源，不再承诺 Go 私有的词法格式。
- 工具执行端口现在把 Provider 产生的真实 `toolCallId` 一直传到单工具和命名工具实现，不再在
  `AgentLoopToolAdapter` 处丢弃原版 `AgentTool.execute` 可见的调用身份。
- 已建立 `queue_clear_abort_settled` 共同 workflow，逐字段对齐 streamingBehavior steer/followUp、
  `all`/`one-at-a-time` 混合 drain、queue recall、abort 后继续处理存活队列、4 次 Provider 输入、
  69 个事件、11 条 JSONL entry、最终 idle/settled 及 reopen。
- 该场景修复了公开 `queue_update` 事件在观察者克隆后把空数组降为 `nil/null` 的偏差；两类队列
  现在始终保持原版数组契约，同时仍为每个观察者提供独立副本。
- 已建立 `provider_retry_after_recovery` 共同 workflow，逐字段对齐 2 次 Provider 输入、29 个事件、
  5 条 JSONL entry、失败历史持久化与 retry live context。Agent 高层退避不再重复套用 Provider
  已消费的 Retry-After，`auto_retry_start` 保留真实 assistant 错误文本；真实 HTTP adapter 若错误
  来自不可信响应 body，则使用单独的脱敏 retry lifecycle 文本，避免回显请求 secret。
- 已建立 `manual_compaction_reopen` 共同 workflow，逐字段对齐真实摘要 Provider 请求、26 个事件、
  `compact()` 结果、7 条 JSONL entry、compactionSummary 当前上下文及 reopen。压缩后 token 估算、
  摘要 transport 隔离和显式 `fromHook:false` 已与原版一致。
- 已建立 `context_overflow_compact_continue` 共同 workflow，逐字段对齐 4 次 Provider 输入、45 个
  事件、8 条 JSONL entry，以及“失败持久化但立即重发上下文排除 → 自动摘要 → continue 恢复 →
  reopen 保留完整历史”的状态差异。
- 已建立 `turn_snapshot_model_tools_reload` 共同 workflow，逐字段对齐 3 次 Provider 输入、62 个
  事件和 10 条 JSONL entry。首个运行中请求保持旧 model/low/旧 prompt/旧工具快照；控制切换后，
  同一 run 的下一 tool turn 立即使用新 model/high/新工具；reload 刷新 resource prompt，同时保留
  Agent、消息、模型/思考选择、工具选择和 reopen 状态。
- 已建立 `tree_navigation_runtime_fork` 共同 workflow，逐字段对齐 4 次 Provider 输入、源会话 54 个
  事件与 fork 会话 20 个事件。无摘要 navigation 把用户目标文本送回编辑区并把叶节点移到其父项，
  废弃分支仍完整留在源 JSONL；默认 `fork(before)` 通过 Runtime.Factory 替换 AgentSession，新会话
  记录源文件为 `parentSession`，源/分支的 entries、tree、context、物理 JSONL 与 reopen 全部一致。
- 已建立 `damaged_session_resume_continue` 共同 workflow，逐字段对齐含 malformed line 与 orphan
  parent 的既有 JSONL、Runtime 初始化、1 次 Provider 输入、19 个事件、8 条有效 entry 及 reopen。
  坏行不会在打开或续写时被静默重写，orphan 作为独立根成为当前分支，新增 thinking/user/assistant
  记录沿该分支持久化，原始物理前缀和坏行位置保持不变。
- 已建立 `resource_image_budget_request_assembly` 共同 workflow，逐字段对齐 3 次 Provider 输入、
  65 个事件和 8 条 JSONL entry。skill 与 prompt template 先展开为 durable user content；
  `images.blockImages` 在每次 Provider 转换时动态过滤完整上下文并合并连续占位文本，但不改写
  user/tool rich image、事件或 session；关闭后既有历史图片与新图片重新进入请求；四档
  `thinkingBudgets` 随每次请求传递，reopen 保留原始 rich content。
- AgentLoop 现在保留并接受 Provider 实际返回的 assistant provenance，不再要求它与请求 model
  alias 完全相同；流内 provenance 一致性仍由 collector 校验。这与原版允许 backend 报告实际
  resolved model 的行为一致。
- 静态 `CreateAgentSession` 现在和带动态 ModelRuntime 的生产装配一样应用 retry budget/base delay、
  compaction reserve/keepRecent 与 branch-summary reserve，不再静默回退默认值。

### 产品级 AgentSession

- settings、resources、queue/retry/compaction 参数及 active tools/system prompt 的 reload 已接入；
  production 的内置 tool/standalone bash runtime 已按新设置整代重建并保留 active tool 名称。
- `images.blockImages` 已接入最终 AgentMessage→Provider 转换并动态读取有效设置；settings
  `thinkingBudgets` 在 Session 创建时进入 stream contract；额外 prompt/skill 路径已进入 production
  resource assembly，并在 reload 时与新资源 snapshot 原子替换，失败仍保留上一健康代。
- Provider reset、动态 extension runtime、`user_bash` hook dispatch、flag 保留和扩展资源二次发现
  尚未完成，但 Provider/插件宿主当前明确后置，不阻塞第一轮 Agent workflow 等价验收。

### 非阻塞覆盖面与内置工具

- production 已支持 OpenAI Responses、OpenAI Chat Completions、OpenAI Codex Responses
  （含 WebSocket）和 Anthropic Messages；完整 provider catalog/composition、auth/status/login、
  动态模型目录和全部 API dialect 当前后置。
- read image、edit normalization、grep/find/ls、glob/gitignore 已有真实实现和 upstream fixture；
  仍需更多跨平台与跨实现验收。
- SessionManager 已容忍坏行和 orphan，并有 TypeScript golden；生产 AgentSession 的坏行/orphan 恢复
  与续写也已通过共同 workflow。unterminated tail、无效 UTF-8 和结构损坏仍由更广的 SessionManager
  golden/本地恢复测试覆盖，后续可继续扩大产品级组合语料。

### Runtime 与外部边界

- transport-neutral Host 已建立权威 state snapshot、跨 reload/session replacement 的单一递增
  event stream；`prompt` 在原版 preflight 成功边界确认，实际 Agent run 异步继续，Host 不用
  事件反推核心状态。
- Host 已覆盖 pi-web 当前除 extension UI 外直接调用的 Agent 命令，包括 queue、model/thinking、
  tools、compaction/retry、bash、stats/name、tree/fork、resources 和 reload；命令直接委托现有
  Runtime/AgentSession owner，没有 Host 侧第二套产品逻辑。
- canonical AgentMessage/ToolResult/usage/event wire 与长期 stdio JSONL transport 已实现；
  `cmd/pi-go-rpc` 使用生产 assembly 打开或恢复 Runtime，并在 EOF/signal 时等待已接纳命令和
  Host 生命周期完整收束。
- Host 尚未覆盖原版 RPC 的全部辅助命令（例如 cycle/list/new/switch/clone/entries/tree/messages）；
  多 session registry 已由 `internal/application.Supervisor` 实现，每个会话仍持有独立
  Runtime/Host，不合并 Agent 状态。
- 原生 `pi-go-web` 已完成静态资源装配、进程内 Session supervisor、共享 Host JSON projection、
  Agent command API、SSE、session list/restore/context/tree/state/rename、基础 models 与 cwd API。
  DeepSeek V4 Flash 已通过一次真实 `read` 工具短程验收。
- `surface/web` 已高内聚拥有 React、HTTP/SSE、静态资源和 Web 测试；日常开发使用 Next HMR
  代理 API-only Go 进程，不执行 `npm build`，生产构建与运行命令彼此独立。
- WebUI 未实现能力继续由 `docs/WEBUI.md` 和结构化 unsupported 响应记录；当前不进入 Agent 主线。

### 整体验收

当前跨实现 golden 已覆盖 SessionManager、resource、tool 的局部场景，以及九个完整
AgentSession workflow。尚未共同验证：

- settings/resource reload、thinking budget 生命周期和运行中 turn snapshot 的组合；
- retry/compaction 与 abort、reload、tree navigation 的更复杂竞争；
- 上述场景的完整 event、usage/cost、错误分类和 session JSONL。

在这些场景通过前，不能称为“完整 Agent 重写”。

## 验证基线

最近一次本地完整审计通过：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `go build ./...`
- `make web-check` 与 `make web-build`（均委托 `scripts/webui.sh`；静态前端构建与 tagged Go build）
- `git diff --check`
- 固定上游 `createAgentSession()` 的 rich/tool/reopen、queue/abort/settled、retry/Retry-After、
  manual compaction、overflow compact/continue 与 model/thinking/tools/reload turn snapshot
  workflow，以及 tree navigation/Runtime fork、damaged session resume/continue、
  resource/image/thinking-budget request assembly oracle；
- SessionManager TypeScript/Go golden check；
- WebUI typecheck、lint、static build，以及真实 Host fixture 的 prompt/SSE/persistence/restore/fork；
- 67 个复用前端源码文件与当前 pi-web 基线逐文件一致。

已完成的大模块 live 验收包括 DeepSeek V4 Flash 的 OpenAI Responses、Chat Completions、Anthropic
Messages 三协议真实 Agent tool-loop，以及原生 `pi-go-web` HTTP/SSE → Host → Runtime → `read` 工具的
短程只读任务。旧 pi-web adapter 验证只作为历史可行性证据，不计入当前产品路径。

远端 Provider 测试为显式 opt-in，无凭据时跳过。生产代码未发现静态模型回复或 fake tool
result 冒充真实实现。

后续验证默认使用单元/集成测试和短程只读任务；只有完成一个高内聚大模块后才进行 DeepSeek
真实验收，避免用模型网络延迟替代本地反馈循环。
