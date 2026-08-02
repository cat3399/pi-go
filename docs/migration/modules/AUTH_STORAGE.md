# M-AUTH：API-key storage 与 runtime charter

状态：`implemented; independent review pending`（`M-AUTH/v0.2-openai-codex-oauth`）

最近通过里程碑：`M-AUTH/v0.1-api-key-storage-runtime`

## v0.2 增量负责

- OpenAI Codex（ChatGPT）OAuth 的 cryptographic PKCE/state、授权 URL、localhost callback、手工回贴和 device-code login service；服务只返回 URL 或接受显式注入的 browser opener，绝不在 production assembly 自动打开浏览器；
- token authorization-code exchange / refresh 的 HTTP、strict bounded UTF-8 JSON、JWT account metadata 与 secret-safe typed failures；
- OAuth expiry/skew、同 provider 双检 single-flight refresh、rotation 持久化和 delete/logout 与 runtime API-key override ownership；
- stored OAuth 到 OpenAI Responses preflight 的 provider-ready bearer credential projection。

## 负责

- `auth.json` 的 API-key credential read/set/delete/list，单文件的进程内与健康进程间串行化；
- Unix 0600 private-file admission、Windows persistent-auth fail-closed、unknown provider record 保留、
  strict malformed rejection；
- 临时文件 `fsync`、atomic rename 和尽力 directory sync 的 durable replacement；
- request-lifetime runtime API-key overlay，以及 CLI/runtime、stored、configured、ambient 的明确 precedence；
- literal、`$NAME`/`${NAME}` template 和 `$$`/`$!` 转义解析，typed secret-safe errors。

## 不负责

- Anthropic、GitHub、Copilot 或其他 provider OAuth；真实浏览器 smoke、interactive TUI/CLI surface；
- models catalog、provider request/auth protocol、settings/project trust；
- command-backed configuration execution。固定上游使用 shell 执行，但当前产品没有可接受的
  command trust、cwd、environment disclosure 与 cross-platform process-tree contract。因此 v0.1
  对 leading `!` 明确拒绝且不启动进程；在上述 contract 和 bounded/cancellable lifecycle 一起
  设计并测试前不得放开。

## 上游证据

- `packages/coding-agent/src/core/auth-storage.ts`；
- `packages/coding-agent/src/core/runtime-credentials.ts`；
- `packages/coding-agent/src/core/resolve-config-value.ts`；
- `packages/ai/src/auth/{types,resolve,helpers}.ts`；
- `packages/coding-agent/test/{auth-storage,runtime-credentials,resolve-config-value}.test.ts`，基线
  `a116523434806910336b9de3e38a41aa5860030b`。

## Ownership 与 invariant

- `internal/auth` 独占 auth-file parsing/writing、runtime overlay 和 secret resolution；application
  只选择 OpenAI/model route 并把结果装配进 provider。
- 写操作先取得 context-aware process-local semaphore，再取得 sibling `.lock` directory；该目录由
  atomic `mkdir` 获得，因此正常的多个 Go process 会串行化。等待任一层时取消或 deadline 都返回
  typed error，并释放已经取得的层级。
- 不会自动回收残留 `.lock`：无法可靠证明原持有者已经死亡时，抢锁会比可诊断的停顿更危险。
  这构成目前跨进程可移植边界；operator 可在确认无存活 writer 后移走残留 lock directory。
- 每次 mutation 在锁内重读 root object；unknown provider raw JSON value 会参与下一次编码，不能因
  写入另一 provider 而丢失。malformed/duplicate/non-object/unsafe-mode input 绝不覆盖。
- replacement 在同目录 temporary 0600 file 完成 write+sync+close 后 rename，再同步 directory。
- v0.1 没有实现 Windows DACL admission/creation，因此 Windows 上 missing auth.json 可继续使用
  runtime/configured/environment source；任何 persistent set/delete 都返回 typed unsupported，存在
  auth.json 时 read/list 返回 typed permission error。不得用 mode bits 冒充 Windows ACL 保证。
- error text 只包含 operation/category/provider ID，绝不拼接 key、raw JSON、environment value 或
  command text；cause 可供 `errors.Is/As` 使用。

## Behavior slices 与 test evidence

| ID | 行为 | 状态 | 证据 |
| --- | --- | --- | --- |
| `B-AUTH-001` | API-key read/set/delete 保留 unknown provider，malformed 不覆盖 | `ported` | strict JSON、round-trip、preservation matrix；R-AUTH-001 |
| `B-AUTH-002` | private admission、atomic/durable replacement；Windows persistence fail-closed | `ported` | mode、Windows-only runner suite/cross-compile、fault cleanup；R-AUTH-001 |
| `B-AUTH-003` | context-aware same-process and cross-process locking | `ported` | same/different Store、two re-exec writers、cancel/failure release/merge、`-race`；R-AUTH-001 |
| `B-AUTH-004` | runtime override 与 stored/configured/ambient source ownership | `ported` | overlay/precedence/error matrix；R-AUTH-001 |
| `B-AUTH-005` | config template; command value safe refusal | `ported` | escapes, missing env, no-process-side-effect；R-AUTH-001 |
| `B-AUTH-006` | PKCE/state, browser callback/manual code lifecycle | `implemented` | bind/state/error/cancel/late callback/opener seam |
| `B-AUTH-007` | device code and bounded token exchange/refresh | `implemented` | pending/403/slow_down/status/UTF-8/size fixture |
| `B-AUTH-008` | OAuth refresh locking, rotation durability and ownership | `implemented` | concurrent resolve, write fault, no fallback, runtime override |
| `B-AUTH-009` | production OAuth→Responses preflight | `implemented` | local token + SSE fixture and rotated auth.json |

## v0.1 review gate

R-AUTH-001 已完成独立联合审查与定点复审，最终结论 `passed`，Blocker/Major/Minor 为
0/0/0。Windows DACL-backed persistence 和 Windows 实机 test execution 未作为已完成能力；
v0.1 在 Windows 保持上述 fail-closed contract。

OAuth and bounded command execution are deferred to their own explicitly reviewed slices. Re-evaluate command
execution only with a concrete trust decision, bounded process-tree implementation for each supported platform,
and no-secret diagnostics tests.

## v0.2 ownership, evidence and acceptance

- `Store` remains the only durable owner. OAuth refresh re-reads expiry under its existing process-local and cross-process locks; exchange or atomic write failure never replaces the old credential.
- Explicit/runtime API keys beat stored OAuth. A selected malformed, failed-refresh or failed-persist OAuth record never falls through to models.json or ambient keys.
- Callback accepts only matching-state `GET /auth/callback` with code; bind/state/error/cancel/late callback cannot settle another transaction. The returned authorization transaction owns listener lifecycle.
- OAuth bodies, codes, verifier and refresh token never enter errors/logs. Windows keeps v0.1's fail-closed persistent-store admission; it does not fake a DACL guarantee.

Fixed upstream evidence: `a116523434806910336b9de3e38a41aa5860030b`,
`packages/ai/src/auth/{types,resolve,helpers,context}.ts`,
`auth/oauth/{openai-codex,pkce,device-code,oauth-page,load}.ts`, coding auth-storage and
`openai-codex-oauth`/`oauth-auth`/`oauth-device-code` tests. Real-browser smoke remains `deferred` pending explicit credential and browser authorization; independent review is pending.
