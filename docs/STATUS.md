# 当前状态

本文只描述当前代码事实，不记录历史迁移过程。

## 总体判断

pi-go 已经不是原型。真实生产路径为：

`Runtime → AgentSession → Agent → AgentLoop → Provider/Tools`

AgentSession 已组合 SessionManager，生产入口没有使用 scripted/fake Provider 冒充真实模型。
AgentLoop、stateful Agent 和当前生产版 SessionManager 的主体完成度较高；AgentSession 与
Runtime 已实现大量产品能力，但尚未达到与原版等价的整体验收。

当前处于“补齐产品级 AgentSession 和 Runtime 边界，然后执行跨实现整体验收”的阶段。

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
- 原位 reload 已按 `session_shutdown → settings → queue/runtime settings → resources →
  active tools/system prompt → rebind → session_start` 顺序实现，并保留运行中请求的 turn snapshot。

### Runtime、Provider、工具和服务

- create/switch/new/fork/import/dispose replacement 生命周期，以及不替换 AgentSession 的 Runtime reload；
- OpenAI Responses 与 Chat Completions 的真实 HTTP adapter；
- bash/read/write/edit/grep/find/ls 七个真实本地工具；
- settings、auth、model、resource、prompt、skills 和 trust 基础服务；
- deterministic fake、fault injection 和本地 HTTP fixture 测试设施。

## 尚未对齐

### Agent 状态与事件

- Agent 已是 model/thinking/system prompt/active tools/messages 的运行时唯一事实源；AgentSession
  只保留完整工具目录和构建 system prompt 所需的产品元数据。
- `agent_settled` 回调观察到的是 idle session，并允许立即开始下一次 prompt。
- 仍需通过跨实现用例继续验证 queue、abort、retry、compaction、branch-summary 和扩展回调的
  完整组合顺序。

### 产品级 AgentSession

- settings、resources、queue/retry/compaction 参数及 active tools/system prompt 的 reload 已接入；
  Provider reset 暂按当前阶段约定后置，动态 extension/tool runtime 重建、flag 保留和扩展资源二次发现
  仍未实现，因此尚不能称为原版完整 reload。
- 缺少 standalone bash 及其流式更新、abort 和 BashExecution session 记录。
- extension-neutral command/input 契约已经接入 AgentSession；完整 JS extension loader、动态失效与
  command context actions 属于后续 Runtime/扩展宿主工作。

### Provider、Model、Auth 和内置工具

- production 已支持 OpenAI Responses、OpenAI Chat Completions、OpenAI Codex Responses
  （含 WebSocket）和 Anthropic Messages；完整 provider catalog/composition 暂不在当前阶段处理。
- provider auth/status/login、动态模型目录和原版全部 API dialect 仍未齐全。
- read image、edit normalization、grep/find/ls、glob/gitignore 已有真实实现和 upstream fixture；
  仍需更多跨平台与跨实现验收。
- SessionManager 已容忍坏行和 orphan，并有 TypeScript golden；复杂损坏恢复仍需扩大共同语料。

### Runtime 与外部边界

- AgentSession 已有 extension command dispatch；Runtime/Host 尚无覆盖内置命令的统一 dispatch、
  权威 state snapshot、canonical wire DTO 和单一有序 event stream。
- 当前 CLI 只执行一次 prompt 后退出，没有长期 JSONL RPC host。
- pi-web 仍在服务端直接使用 TypeScript AgentSession、SessionManager、model 和 auth 服务。

### 整体验收

当前跨实现 golden 主要覆盖 SessionManager 的少量场景。尚未共同验证：

- 连续对话与 rich input；
- queue、retry、abort 和 compaction 竞争；
- model/thinking/tools/reload；
- fork、tree、reopen 和损坏恢复；
- 完整 event、usage/cost、错误分类和 session JSONL。

在这些场景通过前，不能称为“完整 Agent 重写”。

## 验证基线

最近一次完整审计通过：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `go build ./...`
- `git diff --check`
- SessionManager TypeScript/Go golden check

远端 Provider 测试为显式 opt-in，无凭据时跳过。生产代码未发现静态模型回复或 fake tool
result 冒充真实实现。
