# M-APP：Coding-agent application 与 headless CLI charter

状态：`ported`（`M-APP/v0.3-production-tool-replay`；`R-PROVIDER-005` passed）

最近通过里程碑：`M-APP/v0.3-production-tool-replay`

通过里程碑：`M-APP/v0.3-production-tool-replay`。production assembly 以同一个 immutable
built-in registry 生成 Agent executor 与 provider tool definitions；与 M-AGENT/v0.2 联合集成后，
本地 HTTP/SSE oracle 固定验证 built-ins `strict:false`、`parallel_tool_calls:true`、两个 Bash
call 并发执行、逆序完成但 source-order durable replay。全局 sequential 或已广告 tool-level
sequential override 会在 request boundary 发送 `false`。该扩展不改变 auth/model/resource
selection，也不引入 scripted、credential 或 TypeScript fallback。Provider+rich integration
另以 close/reopen oracle 固定同一请求中的 tools/parallel、causal outputs、reasoning/text
metadata、foreign safe projection 与 PNG；context/retry production wiring 仍待后续合并。

## 负责

- 组合 M-PROVIDER、M-AGENT、M-SESSION 与 M-TOOL；
- 显式 text print/headless prompt、session path、stdout/stderr 和 exit code；
- application signal/cancellation、flush、close 和依赖 teardown；
- WF-001 的跨模块验收与重新启动/resume。

## 明确不负责

- Provider protocol、tool process 内部实现或 session wire format；
- 首里程碑之外的 interactive/TUI、JSON event mode、RPC、自动 non-TTY mode、`@file`、
  multiple positional prompts、extension output、session picker 或 model/auth selector；
- 用 production TypeScript fallback 或隐藏 test hook 补齐未迁移功能。

## 上游证据

- `packages/coding-agent/src/main.ts` 的 mode selection、initial message 与 runtime 装配；
- `packages/coding-agent/src/modes/print-mode.ts::runPrintMode`；
- `packages/coding-agent/test/print-mode.test.ts`；
- `packages/coding-agent/src/core/agent-session-runtime.ts` 的 replacement/teardown；
- `packages/coding-agent/test/session-file-invalid.test.ts`；
- `packages/coding-agent/test/suite/harness.ts` 与 prompt/bash persistence scenarios。

## Contract 与 ownership

- v0.1 接受显式 `-p <prompt>` 与可选显式 session path；CLI parsing 只实现当前 workflow
  需要的参数，不预冻结全部上游 flags。
- Application 创建/打开 session、装配 provider/agent/bash，发起 prompt，等待 run 与
  durable flush settle，最后关闭依赖。下层模块不反向依赖 CLI 或 terminal。
- Text mode stdout 只写最终 assistant 的 text block，每 block 追加换行。Thinking、tool
  output、session diagnostics 和 error 不直接写 stdout。
- Terminal assistant `error` 或 `aborted` 的受控 message 写 stderr、无 stack，exit 1；
  正常完成 exit 0。
- `SIGINT` cancel/cleanup/flush 后 exit 130，`SIGTERM` 为 143，Unix `SIGHUP` 为 129。
  上游 print mode 没有显式 SIGINT handler，pi-go 将其作为生命周期加强。
- Cleanup 完成前仍视为 application active；provider/tool goroutine 或 stdout writer 不得
  在 Run 返回后继续活动。
- 显式打开 invalid/corrupt/future session 时保留原文件、输出可诊断错误并失败退出；
  application 不自动覆盖或“修复”。
- Production `cmd/pi-go` 调用 `app.RunProduction`，injected workflow test 调用 `app.Run`；
  两者在 admission/assembly 后进入同一个 lifecycle path。
- Fake 只通过 `_test.go` 中的 Go test re-exec helper 注入：helper 子进程重新执行测试
  binary 的固定 `TestProcessHelper`，由 test-only environment marker 选择 deterministic
  fixture，再调用同一个 production `app.Run`。该 factory 编译在 test binary 中，不存在
  于 release binary，也不是普通 CLI flag/provider fallback。
- 这条 seam 为 v0.1 提供真实 subprocess、signal、stdout/stderr、exit 与 teardown evidence。
  标准 provider adapter 可由本地 HTTP fixture 驱动后，再增加 production binary 的
  request/stream smoke；它不替代 v0.1 process test。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-APP-001` | `-p` 成功只输出 final text，stderr 为空，exit 0 | WF-001 | `ported` |
| `B-APP-002` | terminal provider error/abort 写 stderr、exit 1 并完整 teardown | WF-001 | `ported` |
| `B-APP-003` | SIGINT/SIGTERM/SIGHUP cancel、reap、flush，退出后无 late output/commit | WF-001 | `ported` |
| `B-APP-004` | 新 application 实例按 path resume WF-001 durable conversation | WF-001 | `ported` |

## v0.1 退出与 review gate

- WF-001 使用同一 application Run path、deterministic fake、真实 M-SESSION 和 Bash fake/
  integration runner完成；旧对象全部释放后由新实例 resume；
- stdout、stderr、exit code、signal 与 teardown 有 integration/process evidence；
- Production binary 不包含 TypeScript 调用、runtime fallback 或只为测试开放的隐式
  provider selection；
- [../REVIEWS.md](../REVIEWS.md) 中 M-APP 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。

v0.1 的 deterministic application path 已完成；fake 只存在于 `_test.go` re-exec 路径。

## v0.2 production assembly 边界

- 一个整体里程碑完成 OpenAI model CLI 选择、API-key source precedence、默认 session
  path、标准 Responses adapter 注入和 production print workflow，不按配置文件拆开 review。
- `--api-key` 是不持久化的 request-lifetime override；只读优先级为 `--api-key`、
  `auth.json`、`models.json providers.openai.apiKey`、`OPENAI_API_KEY`。失败必须在
  session/network 副作用前返回，诊断不得包含 secret。
- 当前只装配固定的 `openai/openai-responses` dialect；未知 provider、非法 model route 和
  unsupported stored credential 明确失败，不 fallback 到 scripted 或其他 runtime。
- 默认 session 位于 agent dir 下按 cwd 隔离的 sessions 目录；`--session` 仍显式覆盖。
- OAuth/login、credential 写入的产品命令、命令型 config value、完整 models/catalog、settings/project
  trust selector/interactive flow 与真实 credential smoke 分别延期；`M-RESOURCE/v0.1` 现已在
  session/network 前装配 trusted system prompt 并展开 admitted `-p /template`，且已通过 `R-RESOURCE-001`；selected OpenAI 配置中的
  非投影字段会明确失败，不能被静默忽略。

| ID | 行为 | Workflow | 当前状态 |
| --- | --- | --- | --- |
| `B-APP-005` | release path 装配标准 OpenAI Responses 并完成一次 text print run | WF-002 | `ported` |
| `B-APP-006` | CLI/stored/configured/environment API key 优先级与 secret-safe preflight | WF-002 | `ported` |
| `B-APP-007` | OpenAI model route、provider-prefixed model 与 unsupported route 诊断 | WF-002 | `ported` |
| `B-APP-008` | cwd 隔离的默认 durable session path，显式 session path 优先 | WF-002 | `ported` |
| `B-APP-010` | production OpenAI multi-tool concurrent execution、source-order durable rich replay/restart 与 final print | WF-003 | `ported` |

退出 gate：本地 HTTP/SSE production workflow、配置 fuzz、全仓 test/vet/race/build、
跨平台 compile 和 `R-APP-002` 均通过。

`B-APP-010` 的 provider/Agent/base 基线分别来自 `R-PROVIDER-005`、`R-AGENT-002` 与
`R-BASE-003`；联合 integration oracle 只证明组合边界，不重写三个历史 review 的原范围。
