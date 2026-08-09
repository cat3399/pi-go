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
- stats、context usage、last assistant text 和 runtime policy controls。

### Runtime、Provider、工具和服务

- create/switch/new/fork/import/dispose replacement 生命周期；
- OpenAI Responses 与 Chat Completions 的真实 HTTP adapter；
- bash/read/write/edit/grep/find/ls 七个真实本地工具；
- settings、auth、model、resource、prompt、skills 和 trust 基础服务；
- deterministic fake、fault injection 和本地 HTTP fixture 测试设施。

## 尚未对齐

### Agent 状态与事件

- AgentSession 与 Agent 仍重复保存部分运行配置，尚未完全收敛到单一事实源。
- `agent_settled` 发出时 session 仍处于 settling/busy。
- Continue 的部分 queue drain 路径、非 assistant hook/event 顺序、branch-summary phase 和少数
  产品事件尚未与原版完全一致。

### 产品级 AgentSession

- all-tools registry、active-tools 和 system prompt 联动重建不完整。
- prompt template、skills/commands 和 input preprocessing 尚未统一进入 AgentSession。
- 缺少 settings/resources/providers/tools/queue modes 的完整 reload。
- 缺少 standalone bash、流式更新、abort 和 BashExecution session 记录。

### Provider、Model、Auth 和内置工具

- production 仅支持两个 OpenAI API dialect；完整 provider catalog/composition 尚未实现。
- models.json override、provider auth/status/login 和部分 stream options 尚未完整生效。
- production 尚未注册原版独立的 `openai-codex` provider 和
  `openai-codex-responses` adapter。
- read image、edit normalization、glob/gitignore 等工具行为仍有差异。
- SessionManager 对坏行、orphan 和文件大小使用更严格策略，尚未完成兼容决策验收。

### Runtime 与外部边界

- 尚无统一 command dispatch、权威 state snapshot、canonical wire DTO 和单一有序 event stream。
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
