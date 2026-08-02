# M-TOOL：Tool 与系统能力 charter

状态：`ported`（`M-TOOL/v0.1-bash`）

首个里程碑：`M-TOOL/v0.1-bash`

## 负责

- 内置 Bash tool 的参数、working directory、执行、输出、timeout、cancel 和 cleanup；
- stdout/stderr capture、tail limit、完整输出 artifact 与 ToolResult；
- subprocess/process-group adapter 的跨平台生命周期；
- tool settle 后 late output/update 的隔离。

## 明确不负责

- Agent/session append、provider continuation、CLI stdout/stderr；
- 把 working directory 宣称为 filesystem sandbox/root；
- 首里程碑之外的 read/write/edit/find/grep/ls、直接用户 `!bash`、remote operations、
  extension spawn hook、自定义 renderer 或 shell command prefix。

## 上游证据

- `packages/coding-agent/src/core/tools/bash.ts`；
- `packages/coding-agent/src/core/tools/truncate.ts`；
- `packages/coding-agent/src/core/tools/output-accumulator.ts`；
- `packages/coding-agent/src/utils/child-process.ts` 与 `src/utils/shell.ts`；
- `packages/coding-agent/test/tools.test.ts` 的 bash suite；
- `packages/coding-agent/test/suite/regressions/5208-late-bash-output.test.ts` 与
  `5303-bash-output-truncation.test.ts`；
- `packages/agent/src/agent-loop.ts` 的 tool failure -> ToolResult 归一。

Test intent 由 [../TESTS.md](../TESTS.md) 追踪。

## Contract 与安全边界

- 输入为 `{command: string, timeout?: number}`。Timeout 单位秒，必须 finite、`> 0`，
  最大 `2147483.647`；未提供时没有默认 timeout。
- Tool 构造时绑定 active session `WorkingDir`，model 不能在单次 call 中改变 cwd。
  执行前验证目录存在。
- 上游 Bash 没有 filesystem confinement：command 可以使用绝对路径或 `..` 访问当前
  OS account 已有权限允许的资源。pi-go v0.1 保持该产品能力并明确风险；它不是
  sandbox，也不能以 `Root` 命名误导调用者。
- Environment 在 application 装配时复制为 immutable snapshot，v0.1 与上游兼容地继承
  除 `PI_SESSION_ID`、`PI_SESSION_FILE`、`PI_PROVIDER`、`PI_MODEL`、
  `PI_REASONING_LEVEL` 外的全部 parent environment；不注入 session metadata。因此 PATH、
  proxy 和 credential env 都会对 model 发起的 command 可见。这是明确的高权限边界，
  未来改为 allowlist 必须作为用户可见的 incompatible security behavior 单独设计。
- Shell 不读取 login profile：显式 injected shell path 优先；Unix 依次 `/bin/bash`、
  PATH 中 `bash`、PATH 中 `sh`，以 `-c` 执行；Windows 依次显式 path、已知 Git Bash
  path、PATH 中 `bash.exe`，找不到则 typed spawn/setup failure。Injected path 不存在时
  不 fallback。测试传入受控 env/shell，不读取测试进程的 credential env。
- Timeout、cancel 和 application signal 必须终止整个可控 process group/tree、关闭本地
  pipe、wait/reap direct child 后才返回。正常 direct shell exit 的 background child 语义
  由下述 idle-grace contract 单独处理，不能声称所有成功 command 都没有 descendant。
- stdout/stderr 按 runner 实际接收顺序合并并使用增量 UTF-8 decoder，不能把跨 chunk
  code point 损坏。成功且空输出表达为 `(no output)`。
- 返回内容保留最后 2,000 行或 50 KiB，以先达到的限制为准；尾部超长单行允许保留
  UTF-8 边界完整的 partial tail。
- 发生 truncation 时，把完整 raw output 写入 application-owned private temp directory；
  Unix directory/file 分别为 `0700/0600`。不能提供等价 owner-only permission 的平台
  必须 fail closed 并返回 typed artifact failure，不能静默创建 broad-readable file。
- 与上游兼容，truncated ToolResult text 包含 absolute full-output path，因此 provider
  context 与 durable session 都能看到并可通过后续 Bash 读取；structured outcome 同时
  保存 path/truncation metadata。M-APP text print 不直接展示 tool result。v0.1 不自动
  删除 artifact：它至少保留到进程退出后，直到用户/OS 显式清理；后续 retention manager
  必须先解决 resumed session 中 stale path，不能默默删除仍被 transcript 引用的文件。
- Non-zero exit、timeout、spawn/cwd failure 形成可判断 execution failure，保留已捕获
  输出并由 Agent 转成关联原 `toolCallId` 的 `isError=true` ToolResult。
- Cancellation 与 timeout 是不同 typed outcome。M-TOOL 只 settle process 并报告
  cancelled，不决定 transcript 或下一 provider round；M-AGENT 负责关联 call、durable
  cancellation terminal 和 continuation。Tool settle 后任何 late output/update 都丢弃，
  不写 session 或 application stdout。
- Bash 不拥有 session，也不能直接 append durable record。

## Direct shell exit 与 background child

为保留 `packages/coding-agent/test/suite/regressions/5303-bash-output-truncation.test.ts`
证明的行为，direct shell exit 后按以下状态机结束 capture：

- stdout/stderr 都 EOF 时立即返回；
- 若 descendant 仍持有 pipe，启动 100ms post-exit idle grace；每个 stdout/stderr chunk
  都重新计时，active descendant 的迟到输出继续收集；
- 100ms 内无新 chunk 时关闭本地 reader 并返回 direct shell exit code。Quiet background
  child 不被正常-success cleanup 杀死，可以继续存活；之后的输出不再被接收；
- 因没有默认 timeout，持续输出的 descendant 可以持续延长等待；调用者需要 timeout/
  cancellation 时必须显式提供。Timeout/cancel 仍杀可控 process group，不走 success
  idle-grace。

这意味着 lifecycle gate 要求“返回后没有仍能 mutation tool state 的 pipe/goroutine”，
而不是“系统中没有 command 主动留下的 background process”。

## Go 重新决策

- Tool 语义与 process runner 分离；runner interface 只包含 Bash consumer 真正需要的
  start/stream/wait/kill 能力，不模拟 Node ChildProcess。
- JSON arguments 在 M-BASE 保真保存，在 M-TOOL boundary decode/validate 为 typed input。
- Internal API 使用 `time.Duration`，JSON/CLI adapter 明确处理 seconds。
- Error 保留 category、exit code、timeout/cancel cause 和 diagnostics，不依赖字符串判断；
  用户文本可根据上游兼容需要单独格式化。
- 真实 shell integration 与 fake runner component test 分层。默认测试不执行不受控命令，
  process cleanup 测试使用固定命令和 temporary working directory。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-TOOL-001` | 固定 WorkingDir 执行成功，合并输出并返回 execution outcome | WF-001 | `ported` |
| `B-TOOL-002` | non-zero、missing cwd 与 spawn failure 的 typed outcome | WF-001 | `ported` |
| `B-TOOL-003` | timeout/cancel kill process tree、wait/reap 且无 late update | WF-001 | `ported` |
| `B-TOOL-004` | 2,000 行/50 KiB UTF-8-safe tail 与 mode 0600 full-output artifact | tool contract | `ported` |
| `B-TOOL-005` | direct shell exit 后 active/quiet background-pipe idle-grace 与 child lifetime | tool contract | `ported` |

## v0.1 退出与 review gate

- Fake runner contract test 与受支持平台真实 shell integration 通过；timeout/cancel/late
  output 路径运行适用 race test；
- Timeout/cancel 没有 runner-owned 遗留 process；所有路径返回后没有仍能 mutation tool
  state 的 pipe/goroutine；正常 background child 仅按 B-TOOL-005 明确存活；
- 文档/API 只承诺 WorkingDir，不虚构 sandbox root；
- [../REVIEWS.md](../REVIEWS.md) 中 M-TOOL 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。
