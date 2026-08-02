# 产品 workflow

Workflow 是跨领域模块的产品验收单位。它证明多个 module contract 能按真实顺序
组合，不替代各模块自己的 unit、component、race、fuzz 或恢复测试。

## WF-001：首个 headless tool loop

状态：`ported`

目标是在不依赖 network、真实 credential 或 TypeScript runtime 的条件下，形成第一条
可重复的 standalone 闭环：

~~~text
text prompt
  -> deterministic provider stream
  -> assistant tool call
  -> one built-in tool execution
  -> tool result enters the next provider context
  -> final assistant text
  -> session save
  -> process/application restart and resume
  -> print mode writes the final text and exits successfully
~~~

### 可观察验收

- 同一个 prompt 只追加一次 user message；provider response、tool result 和最终
  assistant message 按因果顺序进入 conversation state。
- Provider 第一次响应包含一个合法 tool call，tool 执行结果通过 `toolCallId` 关联，
  第二次 provider 调用能看到该结果。
- 成功路径只产生一个最终成功结果；provider error、tool failure 和 cancellation
  分别产生可判断的失败结果，不伪装成空成功响应。
- Session 保存后由新的 application/session 实例恢复；恢复不依赖原进程内对象。
- Text print mode 只把最终 assistant text 写到 stdout，诊断写到 stderr，并用退出码
  区分成功与失败。
- 默认测试完全由 Go fake、受控 filesystem、clock、ID 和 subprocess fixture 驱动。

### Ownership 边界

- Provider 拥有单次 model stream 的产生与结束，不修改 session。
- Agent runtime 是 active turn 的唯一状态提交者，负责 provider event、tool execution
  与下一轮调用的因果顺序。
- Tool 模块拥有子进程或 filesystem 资源生命周期，不直接追加 session record。
- Session 模块拥有 durable conversation 和 append 顺序，不持有 provider stream 或
  terminal state。
- Application 组合依赖、处理 signal/exit/stdout/stderr，不把 CLI 状态下沉到 domain。

### 失败与取消验收

- Provider queue 耗尽或 provider factory 失败时，turn 以明确 provider error 结束，
  不执行 tool，也不继续发起下一轮。
- Tool 参数无效、tool 不存在、执行失败或超时时，产生关联到原 call 的 error result；
  是否继续下一轮由 agent contract 明确决定。
- Provider turn 中取消由 provider 产生唯一 aborted terminal，不执行 tool。
- Tool 执行中取消由 Agent coordinator 等待 tool cancel/回收完成，随后按顺序 durable
  commit 一个关联原 call、`isError=true` 的取消 ToolResult，再 commit 一个唯一
  `aborted` assistant；不发起第二次 provider 调用。该场景的 provider call count 固定为
  1，transcript 为 `user, assistant(toolUse), toolResult(cancelled), assistant(aborted)`。
  这是相对上游“可能再调用一次已取消 provider”的有意生命周期加强。
- 已结束 run 之后不得有 goroutine 继续写 session 或 application 输出；正常成功的
  background shell child 例外地可继续存活，但其 pipe 已与 tool 结果隔离，详见 M-TOOL。
- Session corrupt、partial write 或不支持版本不得被覆盖；恢复失败必须可诊断。

### 组成 slice

WF-001 的直接组成是：

- M-BASE：`B-BASE-001` 至 `B-BASE-004`；
- M-PROVIDER：`B-PROVIDER-001` 至 `B-PROVIDER-003`；
- M-AGENT：`B-AGENT-001` 至 `B-AGENT-005` 及 `B-AGENT-008`；
- M-SESSION：`B-SESSION-001` 至 `B-SESSION-007`；
- M-TOOL：`B-TOOL-001` 至 `B-TOOL-005`；
- M-APP：`B-APP-001` 至 `B-APP-004`。

其中 corrupt/recovery、timeout/cancel 等 behavior 不一定都出现在 happy-path 进程里，
但它们是相同 module contract 的失败路径，不能在宣告 workflow 可用后再补。

实施顺序形成明确 DAG：B-BASE-001 → B-BASE-002 → B-BASE-003/004 →
M-BASE/v0.1 独立 review → deterministic provider text/tool/error → M-PROVIDER/v0.1
独立 review → session/tool → 各自独立 review → agent loop → M-AGENT review →
application/print。只有完整 M-BASE/v0.1 review 通过后才扩大到 provider/tool；同时持续
把已完成部分接入同一个跨模块 scenario，避免横向基础层长期不集成。

WF-001 只有在所有直接组成 behavior 均为 `ported`、跨模块 scenario 通过且各模块
里程碑 review 无 blocker 时才可标记完成。

## WF-002：production OpenAI text print

状态：`ported`

~~~text
CLI provider/model/API-key sources
  -> production dependency assembly
  -> standard OpenAI Responses HTTP/SSE adapter
  -> agent text turn
  -> default or explicit durable session
  -> final text/diagnostic/exit
~~~

验收以本地 HTTP/SSE server 驱动真实 production assembly path，检查 request、auth
precedence、model route、session mutation boundary、stdout/stderr/exit 与 teardown。默认测试不
读取开发机 credential 或访问 network；真实 credential smoke 仍需显式启用。

组成行为为 `B-PROVIDER-005` 与 `B-APP-005` 至 `B-APP-008`。OAuth、完整 model catalog、
tool-call wire、settings/trust 和完整 system prompt 不属于这条 text-only workflow。

本地 production workflow、错误持久化、全仓 quality gate 与 `R-APP-002` 已通过。

## 后续 workflow 规则

新增 workflow 必须说明用户入口、可观察成功与失败、跨模块 ownership、durable data
影响和验收证据。只把多个 unit test 罗列在一起不算 workflow。

## WF-003：production OpenAI single tool replay

状态：`in-progress`（awaiting M-PROVIDER/M-TOOL/M-APP independent review）

~~~text
production OpenAI request (built-in tool schemas)
  -> SSE function_call arguments stream
  -> M-AGENT v0.1 one local tool execution
  -> durable ToolResult in source order
  -> second OpenAI request replays function_call + function_call_output
  -> final assistant text / session
~~~

本地 HTTP/SSE E2E 断言 request1 有 bash/filesystem schemas，call ID 与 `fc_*` item identity 正确归一，empty result 使用 `(no tool output)`，request2 只发送完整 durable state。partial/error/aborted calls 不 replay；unknown/out-of-order/malformed events fail explicit。M-AGENT/v0.2 的 multi-call scheduler 是集成前提，不由本 workflow 宣称完成。
