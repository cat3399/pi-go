# M-AUTH：API-key storage 与 runtime charter

状态：`ported`（`M-AUTH/v0.1-api-key-storage-runtime`）

## 负责

- `auth.json` 的 API-key credential read/set/delete/list，单文件的进程内与健康进程间串行化；
- Unix 0600 private-file admission、Windows persistent-auth fail-closed、unknown provider record 保留、
  strict malformed rejection；
- 临时文件 `fsync`、atomic rename 和尽力 directory sync 的 durable replacement；
- request-lifetime runtime API-key overlay，以及 CLI/runtime、stored、configured、ambient 的明确 precedence；
- literal、`$NAME`/`${NAME}` template 和 `$$`/`$!` 转义解析，typed secret-safe errors。

## 不负责

- OAuth login、refresh、token rotation 或 OAuth credential interpretation；
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
| `B-AUTH-001` | API-key read/set/delete 保留 unknown provider，malformed 不覆盖 | `ported` | strict JSON、round-trip、preservation matrix |
| `B-AUTH-002` | private admission、atomic/durable replacement；Windows persistence fail-closed | `ported` | mode、Windows-only runner suite/cross-compile、fault cleanup |
| `B-AUTH-003` | context-aware same-process and cross-process locking | `ported` | same/different Store、two re-exec writers、cancel/failure release/merge、`-race` |
| `B-AUTH-004` | runtime override 与 stored/configured/ambient source ownership | `ported` | overlay/precedence/error matrix |
| `B-AUTH-005` | config template; command value safe refusal | `ported` | escapes, missing env, no-process-side-effect |

OAuth and bounded command execution are deferred to their own explicitly reviewed slices. Re-evaluate command
execution only with a concrete trust decision, bounded process-tree implementation for each supported platform,
and no-secret diagnostics tests.
