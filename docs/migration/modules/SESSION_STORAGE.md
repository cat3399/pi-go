# M-SESSION：Session 与 storage charter

状态：`ported`（`M-SESSION/v0.1-linear-v3`）

首个里程碑：`M-SESSION/v0.1-linear-v3`

## 负责

- Durable session header、entry identity、parent chain、current leaf 和 context projection；
- domain entry 与 storage record 的显式转换；
- v3 JSONL 的 create、ordered append、flush/sync、open、close 和 resume；
- unknown field/entry 保留，以及 corrupt、partial、unsupported version 的诊断；
- 同一 session 的 single-writer serialization、写失败后的 poison/quarantine 状态与
  不确定提交诊断。

## 明确不负责

- Provider stream、tool execution、CLI resume selector 或 terminal output；
- 首里程碑之外的 discovery、v1/v2 migration、branch/fork/tree、compaction、label、
  custom entry、multi-process locking 或 SQLite；
- 把 AgentHarness 的 `leaf` record/retained-tail wire 与 coding-agent v3 JSONL 假定为
  完全相同格式。

## 上游证据

- `packages/coding-agent/docs/session-format.md`；
- `packages/coding-agent/src/core/session-manager.ts` 的 v1-v3 record、parser、open、append
  与 migration；
- `packages/coding-agent/test/session-manager/`；
- `packages/coding-agent/test/suite/agent-session-bash-persistence.test.ts`；
- `packages/agent/src/harness/session/jsonl-store.ts` 的 strict parser 与 serialized append；
- `packages/agent/src/harness/session/session.ts` 和
  `packages/agent/test/harness/session-backends.test.ts`；
- [../AGENT_PATHS.md](../AGENT_PATHS.md) 对两条高层路径的分类。

具体 durable inventory 见 [../DATA_FORMATS.md](../DATA_FORMATS.md)，test intent 由
[../TESTS.md](../TESTS.md) 追踪。

## v0.1 writer 与 reader 输入域

首行是 coding-agent v3 header：

~~~json
{"type":"session","version":3,"id":"...","timestamp":"...","cwd":"..."}
~~~

WF-001 writer 只创建线性 `message` entry：user、assistant tool-use、toolResult 和 final
assistant。每个新 entry 有唯一 `id`、`parentId` 和 ISO timestamp；空 session 的首
entry parent 为 null，其余新 entry 指向当前 durable leaf。这里的“线性”只限制 pi-go
v0.1 新建 session 和 append 行为，不把 coding-agent v3 tree 格式错误地说成线性 wire。

v0.1 reader 接受以下 coding-agent v3 tree 子集：

- header 必须是第一条非空 record，版本恰为 3；header 后可以没有 entry；
- entry ID 唯一；恰有一个 root (`parentId=null`)；其余 parent 必须引用物理位置更早的
  entry，因此 forward parent、broken parent 和 cycle 都拒绝；
- parent 可以引用非物理 tail，因而合法 branch 会被完整读取；active leaf 是物理最后
  一条 entry，provider context 只沿该 leaf 的 parent chain 投影，非祖先 branch 保留但
  不进入 context；
- 多 root 虽可由上游低层 `resetLeaf()` 构造，但不在 v0.1 兼容输入域；reader 以明确
  `unsupported tree shape` 拒绝且禁止 append，不把它当作可丢弃 branch；
- 在已接受 tree 后，v0.1 writer 只向 active leaf 追加一个 child，不提供 branch/reset
  API。完整 branch/compaction milestone 再扩大 writer 能力。

Message/content/usage 由 M-BASE 表达，但 JSON record 是 M-SESSION 自己的兼容类型。
未知 header、entry、message 和 content 字段必须保留 raw JSON 值。Open 时每条 record
同时保存不可变 raw bytes 与从同一 bytes 得到的 typed validation/projection；loaded
entry 不提供字段 mutation，因此不存在两份可冲突的 authoritative state。

v0.1 的 round-trip oracle 明确定义为 `Open -> Append -> Close -> Reopen`：原文件全部
bytes 必须成为新文件的 byte-identical prefix，只允许在合法无尾换行文件后补一个
separator，再追加新 record。它不提供 decode/re-encode 或 rewrite 操作，也不声称 JSON
key order/whitespace 的 semantic rewrite equivalence。未来 migration/rewrite 必须另建
behavior，逐字段定义 typed-known 与 raw-unknown merge。

Unknown entry 只要有合法 `id/parentId/timestamp` envelope 就参与 tree 和 leaf 选择，但
其 payload 不进入 provider context。Unknown message role 整条保留、整条不投影；known
message role 中的 unknown content block 原位保留，但只投影已知 block，且产生受控
diagnostic。这样 unknown data 不会被当作 prompt/tool 指令执行，也不会因 append 丢失。

## Contract 与 invariant

- Session domain 拥有 header、entry chain、leaf 和 context projection；filesystem
  adapter 拥有 path、JSONL I/O、sync 和 atomic create。
- Storage `Create` 成功表示 session header 已经 durable 创建。Application 可以决定
  何时调用 Create，但 storage 不能用“内存已创建”冒充 durable 成功。
- `Create` 要求 target 的 parent directory 已存在；目录 provisioning 属于 application，
  storage 不在同一次 durable transaction 中隐式创建未同步的 ancestor。
- Append 先完整 encode 到内存，再写入并 sync storage，最后才推进 domain entry/leaf。
  validation/encode 在首次 write 前失败时，内存和文件都不变，writer 仍可用。
- 首次 write 开始后，short write、write error 或 sync error 都保持原内存 leaf，但磁盘
  可能是旧文件、完整未确认 record 或可检测 partial tail。此时 append 返回 typed
  `commit outcome unknown`，writer 立即 write-poisoned，拒绝任何后续 append；调用者
  必须 close/reopen 检查，且不得盲目重试有副作用结果。
- 同一 session 的 append 在进程内串行，concurrent callers 最终只能形成一条可解释
  parent chain。v0.1 明确 single process/single writer，不声称 multi-process safe。
- Open 验证 header、版本、entry envelope、唯一 ID、上述 tree shape 和 parent reference。
  Future version、duplicate ID、broken/forward parent、cycle/multiple root、middle malformed
  或 trailing partial record 都拒绝打开，原文件不变且禁止继续 append。
- 合法但没有尾部换行的最后一条完整 JSON record 可以读取；后续 append 必须先写入
  separator，不能把新 JSON 粘到旧 bytes 后面。
- Unknown entry 参与 parent chain，context projection 可以忽略其未知语义；任何显式
  rewrite/migration 都必须 round-trip raw unknown data。
- Session resume 创建新的 aggregate/application 对象，不依赖旧进程中的 provider、
  tool、goroutine 或 mutable SDK object，也不自动重跑未完成的副作用 tool。

## 上游差异与加强

当前 coding-agent reader 会跳过 malformed line，future `version >= 3` 也可继续打开；
尾部残缺 JSON 被跳过后，下一次直接 append 还可能把新 record 粘到残缺 bytes 后。
这些行为与项目的数据安全硬约束冲突。

pi-go 采用 AgentHarness 更严格的 parse/storage-first 顺序，并进一步保留 unknown header
field、检测 trailing partial、使用可诊断错误和 crash-aware write。相关上游 permissive
test 在 Go ledger 中标记为 `strengthened` 目标，不机械复制 silent skip。

Create 采用同目录 temporary file、file sync、no-replace atomic publication 和适用平台的
directory sync；
append 在 acknowledgment 前 sync。Atomic create 是 pi-go strengthening：上游
`SessionManager.newSession()` 只创建内存状态，实际首次落盘由 `_persist/_rewriteFile`
完成且没有同等 durability contract。

若 create 的 rename 已成功但 directory sync 失败，返回 typed `durability unknown`，不
返回 writable aggregate；目标 path 可能存在，调用者只能显式 reopen，不能覆盖。
Append 的 uncertain failure 如上进入 poison 状态；partial final record 会被 reader 拒写，
完整 record 则可能在 reopen 后出现或未出现，这是 fsync failure 无法消除的不确定性。
显式 recovery/truncate 不属于 v0.1，不能在普通 Open 中自动“修复”。

## Fixture 计划

- `v3-tool-turn.jsonl`：WF-001 四消息链；
- `v3-unknown-data.jsonl`：header、entry、message/content 的 unknown data；
- `v3-branched-tail.jsonl`：一个 root、合法 branch，physical tail 作为 active leaf；
- `v3-multiple-root.jsonl`、`v3-forward-parent.jsonl` 与 `v3-cycle.jsonl`：v0.1 明确拒绝；
- `v3-corrupt-middle.jsonl`：中间 malformed、后面仍有合法 record；
- `v3-trailing-partial.jsonl`：末尾残缺 JSON；
- `v4-unsupported.jsonl`；
- `v3-duplicate-id.jsonl` 与 `v3-broken-parent.jsonl`。

Fixture 使用固定 clock/ID、脱敏内容并记录上游 commit。上游大型历史 v1 fixture 留给
后续 migration/compaction slice，不进入 v0.1。

## 首批 behavior slice

| ID | 行为 | Workflow | 初始状态 |
| --- | --- | --- | --- |
| `B-SESSION-001` | atomic create v3 header，成功返回即 durable | WF-001 | `ported` |
| `B-SESSION-002` | storage-first ordered append 与唯一 parent chain | WF-001 | `ported` |
| `B-SESSION-003` | close 全部旧对象后按 path resume 并重建四消息 context | WF-001 | `ported` |
| `B-SESSION-004` | pre-write failure 不改文件；write/sync failure 不推进 leaf、poison writer 并报告 commit unknown | WF-001 | `ported` |
| `B-SESSION-005` | Open→Append 保持原 byte prefix；unknown raw 保留且 context 安全投影 | compatibility | `ported` |
| `B-SESSION-006` | future/corrupt/partial 与 unsupported tree shape 拒绝且文件不变 | recovery | `ported` |
| `B-SESSION-007` | 同 session concurrent append 串行且通过 race test | recovery | `ported` |

## v0.1 退出与 review gate

- 上述七项由 fixture、round-trip、fault injection 和 `go test -race` 证明；
- Writer pre-write failure 不改变数据；uncertain I/O failure 不推进 leaf、立即 poison，且
  corrupt/future/unsupported input 永不被自动覆盖；
- 没有第二套 AgentHarness session aggregate，也没有把 domain type 直接当 wire type；
- [../REVIEWS.md](../REVIEWS.md) 中 M-SESSION 独立 reviewer 结论为 `passed`，且没有
  unresolved blocker。

v1/v2 migration、tree/compaction 和 multi-process writer 仍是独立里程碑，不被线性
v3 通过结论掩盖。

## M-SESSION/v0.2-tree-branch

状态：`implemented-awaiting-independent-review`

本里程碑扩大 v3 的 domain 输入域为 append-only forest：每个 entry 仍有唯一 ID，非根
entry 仍必须引用物理更早的 parent；因此 broken/forward parent、duplicate ID 和 cycle
继续拒绝，但多个 root 是 `reset leaf -> append` 的合法结果。JSONL 没有 leaf-pointer
record，`SelectLeaf` 与 `ResetLeaf` 是进程内的明确选择；重新打开时一律选择物理最后一条
entry，保证行为可复现且不重写历史。

负责：

- root-to-selected-leaf path、immutable forest snapshot、按 selected leaf 的 context；
- serialized `SelectLeaf`/`ResetLeaf` 与 Append，确保新 entry 精确挂在已选 parent；
- 从一个 selected leaf atomic extract 至新文件，及复制全 forest 的 `ForkFrom`；新 header
  使用新的 ID/cwd/timestamp，`parentSession` 指向 source，source bytes 永不改写；
- create publication、取消和 writer claim 继续沿用 v0.1 的 data-safety contract。

明确延期：`branch_summary`、compaction summary、label/custom/model/thinking entry 的创建和
compaction-aware context。这些 wire entry 可作为未知 entry 保留和走 tree，但没有完整可投影
的 Go domain 语义；不得把它们伪装成已支持的 summary/compaction。

验收重点：branch/reset/reopen round-trip、extract/fork source-preservation、已存在 target 和
post-publication fault、cancel-before-create、forest graph fuzz，以及 selection/append race。
本段不构成独立 review 结论。

上游 `session-manager` test 分类：`tree-traversal` 的 message/tree/path/branch/reset 和
`createBranchedSession` 路径为 ported/strengthened（atomic target 与 source-preservation）；
`build-context` 的 selected-path 部分 ported，compaction/branch-summary projection deferred；
`custom-session-id` 的 Create 既有 coverage 保持，fork custom ID 在本模块 ported；
`file-operations` 的 strict open 已在 v0.1，discovery/list 与空文件初始化属于 application
selector policy deferred；`labels`、`save-entry` 的 extension entry 及 `migration` 的 v1/v2
rewrite 均 deferred，不能因 tree reader 能保留 unknown entry 而宣称其 API 已实现。
