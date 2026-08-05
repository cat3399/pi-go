# 当前状态

本文是 2026-08-05 的工作树实现快照。

## 总体结论

P1 AgentLoop 与 P2 stateful Agent 已完成职责收口。当前执行链只有一套 Provider/Tool 核心：
`AgentSession → Agent → AgentLoop`。AgentSession 负责 durable session、retry、compaction 与
产品配置；Agent 只负责内存状态、监听器、队列和 active run；AgentLoop 负责单次调用中的
Provider streaming、工具执行和 turn lifecycle。

首期整体仍未完成。下一优先级是 P3 SessionManager，随后是 P4 AgentSession 完整产品语义；
RPC 与 pi-web 接入不在当前里程碑。

## 当前实现

| 区域 | 当前实现事实 |
| --- | --- |
| 流式执行 | text/thinking/tool stream、连续 tool loop、多工具并行或顺序调度、取消与 settlement |
| 富内容 | rich user input、image content、rich tool result/details 和队列输入已经进入核心类型与测试路径 |
| 动态运行 | 每个 provider turn 获取 snapshot；model、thinking、system prompt 和 tools 可在 tool chain 间变化 |
| 控制策略 | continue、steering/follow-up queue、retry 与 compaction 的主要机制已经存在 |
| Provider | deterministic fake、OpenAI Responses、OpenAI Chat Completions，以及 reasoning/tool replay 支持 |
| Session 存储 | JSONL v3、锁与原子追加、恢复、tree/branch/fork、compaction、legacy/unknown raw 保护 |
| Agent 核心 | P1/P2 已收口；唯一执行核心、stateful lifecycle、queue、abort、动态 turn snapshot 已有对照测试 |
| 产品基础 | `AgentSession` 已承接持久化、队列、重试、压缩和动态配置，但 P4 完整产品组合语义尚未验收 |
| 工具与服务 | bash/read/write/edit/grep/find/ls，API key/OAuth，settings/model/resource/prompt 加载 |
| 诊断入口 | `cmd/pi-go -p` 可组装当前 production path 并执行一次 headless prompt |

## 当前边界

`internal/agent.AgentLoop` 现在是可直接运行的单次 run 纯内存内核，不依赖 Transcript、
Session、settings、app 或持久化回调。它已覆盖 text/thinking/tool streaming、rich
ToolResult、多轮 tool loop、顺序/并行调度、steering/follow-up、完整 `prepareNextTurn`
replacement、`shouldStopAfterTurn`、动态 API key、provider failure 和原版 lifecycle 顺序。
工具参数严格按 prepare、JSON Schema 转换/验证、before、execute 排序；before 可原地修改或
替换已验证参数，替换值不会被二次 schema 校验。并行批次按源顺序 preflight、按完成顺序
发出 tool end、按源顺序生成 ToolResult。

LLM 消息层现在能保留 `FinishLength + toolCall`，stream、JSONL 与重新计价不会把 stop reason
改写成 toolUse。AgentLoop 和现有 Agent 都会为该消息中的每个 call 生成明确失败结果，不调用
before/execute/after，并继续下一 provider turn。

stateful Agent 已完全通过 AgentLoop 执行。prompt admission、空消息批次、listener failure 的
synthetic lifecycle、string prompt 的 content-block 形状、steering/follow-up mode、active
cancellation signal、动态 API key/convertToLlm，以及 before/message mutation seam 均有行为
测试。Agent 不依赖 Transcript，也不拥有 retry 或 compaction。

尚未完成的核心边界是 P3 SessionManager 的完整产品 workflow、P4 AgentSession 的完整组合
语义，以及后续长期 transport-neutral Runtime。RPC、pi-web、TUI 与 provider breadth 不是
当前里程碑。

## 当前验证状态

当前工作树已通过 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`
和 `git diff --check`。
