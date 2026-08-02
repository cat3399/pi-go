# 独立审查记录

本文件记录领域模块里程碑的独立 review gate。Reviewer 必须未参与被审查里程碑的
实现，并直接检查 diff、测试、ledger、module charter 和固定上游证据。

## 结论格式

每次审查使用稳定 ID `R-<MODULE|STAGE>-NNN`，至少记录：

- 被审查 module、milestone、behavior 和 commit/worktree 范围；
- reviewer 与实现者；
- 实际检查的上游源码、测试、fixture 和本地验证命令；
- 行为正确性、规则符合性、依赖/ownership、并发/数据安全、可继续迁移性结论；
- blocker、需要修正的问题和允许延期的 debt；
- 最终结论：`changes-required` 或 `passed`。

`passed` 只表示声明的里程碑通过，不表示整个领域模块的未来范围已经迁完。

## 未关闭事项

| ID | 严重度 | Owner module | 影响 | 重新评估条件 | 来源 review | 状态 |
| --- | --- | --- | --- | --- | --- | --- |
| `F-STAGE0-001` | Blocker | M-BASE | B-BASE-001 缺可执行 finish/usage validity oracle | successful text/usage matrix 经独立复审 | R-STAGE0-001 | `fix-applied-awaiting-review` |
| `F-STAGE0-002` | Blocker | M-SESSION | append failure 同时承诺原文件不变又承认可留下 partial | poison/commit-unknown contract 经独立复审 | R-STAGE0-001 | `closed-by-R-SESSION-002` |
| `F-STAGE0-003` | Blocker | M-AGENT | tool cancel 后 durable transcript 与 terminal owner 缺失 | provider call count/transcript/settlement oracle 经独立复审 | R-STAGE0-001 | `closed-by-R-AGENT-001` |
| `F-STAGE0-004` | Major | M-PROVIDER | auth stored/ambient precedence 事实错误 | per-handler/per-field merge 规则经独立复审 | R-STAGE0-001 | `fix-applied-awaiting-review` |
| `F-STAGE0-005` | Major | M-SESSION | trust/keybinding/legacy resource 数据清单不完整 | DATA_FORMATS inventory 经独立复审 | R-STAGE0-001 | `fix-applied-awaiting-review` |
| `F-STAGE0-006` | Major | M-PROVIDER | OpenAI Responses text test/source ledger 错位漏项 | request/transport/text/terminal/tool 分类经独立复审 | R-STAGE0-001 | `fix-applied-awaiting-review` |
| `F-STAGE0-007` | Major | M-SESSION | v0.1 reader 对合法 v3 tree 输入域未定义 | reader/writer 子集与 fixture oracle 经独立复审 | R-STAGE0-001 | `closed-by-R-SESSION-002` |
| `F-STAGE0-008` | Major | M-SESSION | unknown round-trip 无可断言操作与 authority | byte-prefix append/projection contract 经独立复审 | R-STAGE0-001 | `closed-by-R-SESSION-002` |
| `F-STAGE0-009` | Major | M-TOOL | env/shell、background pipe、artifact visibility/retention 未决 | Bash 完整 contract 经独立复审 | R-STAGE0-001 | `closed-by-R-TOOL-002` |
| `F-STAGE0-010` | Major | M-APP | deterministic fake 无 process-level test seam | test re-exec 与 production Run path 经独立复审 | R-STAGE0-001 | `closed-by-R-APP-001` |
| `F-STAGE0-011` | Major | M-BASE | module review gate 与 slice 顺序形成文字循环 | 明确 DAG 经独立复审 | R-STAGE0-001 | `fix-applied-awaiting-review` |
| `F-STAGE0-012` | Major | M-SESSION | atomic-create 引用了不存在的上游 symbol | upstream/strengthening 证据经独立复审 | R-STAGE0-001 | `closed-by-R-SESSION-002` |
| `F-BASE-001` | Blocker | M-BASE | 首版 stream 只能形成 text terminal，不能承载完整 tool call | tool start/delta/end 与 tool-use terminal 经复审 | R-BASE-001 | `closed-by-R-BASE-002` |
| `F-BASE-002` | Major | M-BASE | exported zero-value tool call 可通过消息与关联校验 | 所有消费边界重新验证 tool call/result | R-BASE-001 | `closed-by-R-BASE-002` |
| `F-BASE-003` | Major | M-BASE | exported zero-value terminal event 可结束 stream | collector 在状态变更前验证所有 event | R-BASE-001 | `closed-by-R-BASE-002` |
| `F-PROVIDER-001` | Major | M-PROVIDER | response factory panic 绕过唯一 terminal contract | panic 转为单一 typed error terminal 并复审 | R-PROVIDER-001 | `closed-by-R-PROVIDER-002` |
| `F-PROVIDER-002` | Major | M-PROVIDER | queue/factory failure 的 cause/category 被降为字符串 | failure 从 event 贯穿 collector/result 并支持 errors.Is/As | R-PROVIDER-001 | `closed-by-R-PROVIDER-002` |
| `F-PROVIDER-003` | Minor | M-PROVIDER | 极大 ChunkRunes 使容量计算溢出并 panic | MaxInt 回归与无溢出分块计算经复审 | R-PROVIDER-001 | `closed-by-R-PROVIDER-002` |
| `F-PROVIDER-004` | Major | M-PROVIDER | 失败 assistant 的 partial text 被当成 completed history 回放 | error/aborted history 全部跳过且不占 wire index | R-PROVIDER-003 | `closed-by-R-PROVIDER-004` |
| `F-PROVIDER-005` | Major | M-PROVIDER | staged terminal 后的残缺 SSE 尾帧可被 EOF 掩盖为成功 | dirty EOF 转 invalid-response 并保留 staged usage | R-PROVIDER-003 | `closed-by-R-PROVIDER-004` |
| `F-SESSION-001` | Major | M-SESSION | raw arguments 的字节校验不接受上游 parse/stringify 后的语义等价 JSON | exact decimal/escape semantic comparison 与 rewrite/reopen 回归 | R-SESSION-001 | `closed-by-R-SESSION-002` |
| `F-SESSION-002` | Major | M-SESSION | Create 隐式 MkdirAll，但未同步新 ancestor 在父目录中的目录项 | parent precondition 与缺目录回归 | R-SESSION-001 | `closed-by-R-SESSION-002` |
| `F-SESSION-003` | Minor | M-SESSION | 超出 RFC3339 四位年份的 clock 值可写但不可 reopen | create/append 可重开时间验证 | R-SESSION-001 | `closed-by-R-SESSION-002` |
| `F-SESSION-004` | Minor | M-AGENT | bounded settlement 容易被误解为 write 后仍可由 deadline 中断 | 首次 write 线性化边界写入 charter | R-SESSION-001 | `closed-by-R-SESSION-002` |
| `F-SESSION-005` | Major | M-SESSION | context estimate 与 cut-prefix 的 `uint64` 累加可 wrap 低估并写坏 `tokensBefore` | checked overflow、MaxUint64/no-write regression 与 fuzz 经复审 | R-SESSION-004 | `closed-by-R-SESSION-004` |
| `F-TOOL-001` | Major | M-TOOL | queued cancellation 的 relay goroutine 在长 predecessor 下让已返回调用残留 goroutine/barrier | 长 A、批量 B-cancel、C 顺序、settlement 后零 node/key 与 race 定点复审 | R-TOOL-003/R-TOOL-004/R-TOOL-005 | `closed-by-R-TOOL-005` |
| `F-TOOL-002` | Major | M-TOOL | mode write bits 不能表达 effective identity/ACL writability；owner mode `0002` 可被 rename 绕过 | non-mutating effective probe、prepare/commit 双检、`0002`/0444/symlink/TOCTOU 回归复审 | R-TOOL-003/R-TOOL-004/R-TOOL-005 | `closed-by-R-TOOL-005` |
| `F-TOOL-003` | Major | M-TOOL | edit patch hunk/count/context 不可应用 | single/distant multi-hunk 实际 apply oracle 复审 | R-TOOL-003 | `closed-by-R-TOOL-004` |
| `F-TOOL-004` | Major | M-TOOL | malformed ignore rule nil-call panic、I/O error 被吞且缺 parent rule | compiled scoped rules、typed failure、parent/nested/cancel 复审 | R-TOOL-003 | `closed-by-R-TOOL-004` |
| `F-TOOL-005` | Major | M-TOOL | grep context 被 2,000-line cap 截断却报告 byte limit | >2,000 lines 且 <50KiB regression 与 metadata 复审 | R-TOOL-003 | `closed-by-R-TOOL-004` |
| `F-TOOL-006` | Minor | M-TOOL | read NFD fallback no-op，AM/PM fallback 仅大写 | x/text NFD 与 lowercase AM/PM tests 复审 | R-TOOL-003 | `closed-by-R-TOOL-004` |
| `F-TOOL-007` | Minor | M-TOOL | entry I/O/ignore discovery cancellation 不完整 | cancelled empty ls 与 deterministic mid-walk cancel 复审 | R-TOOL-003 | `closed-by-R-TOOL-004` |

## R-STAGE0-001：事实基线首轮独立审查

- 范围：阶段 0 的 source map、scope、provider/data/behavior/test ledger、六个 module
  charter、WF-001 和当时完整未提交文档 diff；不包含 Go 实现。
- 固定上游：`a116523434806910336b9de3e38a41aa5860030b`；reviewer 直接读取
  `/Users/mac/dev/pi` 对应源码、测试、fixture 与 session format。
- 实现者：主任务 `/root`；reviewer：未参与文档编写的独立任务
  `/root/stage0_independent_review`，并分别复核 provider 与 session/tool/workflow 证据。
- 本地核验：复算 495 个 TS/TSX、112,232 行与 package 数量；确认固定 checkout、
  executable AgentSession 路径、provider/dialect/model shard 数量、Markdown link 与稳定 ID。
  审查是只读证据审查，未运行上游测试、Go test 或 build。
- 正确性结论：源码地图、主 agent path、模块 ownership 高层方向、provider/dialect 数量、
  catalog baseline gap 与 strict session/Bash 基础事实通过；3 个 Blocker、9 个 Major 如上表，
  另有路径精度、Cloudflare 术语、ToolResult ownership 与 data 状态语义等 Minor。
- 数据/并发结论：single coordinator、storage-first 和 unknown-preserving 方向成立，但
  append failure、tree 输入域、tool cancel terminal 与 background child contract 未闭合。
- 可继续迁移性结论：架构依赖本身无环；当时的 review gate/实施顺序文字不能照章执行，
  且 B-BASE-001 会迫使实现者发明规则。
- 最终结论：`changes-required`。在所有 `F-STAGE0-*` 经复审关闭前，阶段 0 不通过，
  B-BASE-001 不得进入 Go 实现。

首轮结论后的修订已落在当前 worktree；表中状态只表示 implementer 已提交候选修复，
不表示 reviewer 已接受。复审使用新的稳定 review ID 记录。

## R-BASE-001：M-BASE/v0.1 首轮独立审查

- 范围：`internal/llm` 的 message、usage、tool 与 stream 实现及测试。
- 结论：`changes-required`。完整 tool-call stream 缺失为 Blocker；零值 tool call 与
  terminal event 校验缺口为 Major。usage overflow、terminal 组合和事件终止后的测试
  需要补强，Go 最低版本需更新。
- 修订：候选修复与补强测试已通过 test、vet、build、race 和短时 fuzz，等待定点复审。

## R-BASE-002：M-BASE/v0.1 定点复审

- 范围：R-BASE-001 修订后的 `internal/llm` 完整实现与测试；reviewer 未参与实现。
- 核验：tool stream/terminal、zero-value 防绕过、tool result 关联、terminal/EOF、usage
  overflow、snapshot ownership 与下一 provider 接口可演进性。
- 验证：test、vet、build、race、5 秒 fuzz 均通过；覆盖率 85.1%。
- 最终结论：`passed`，没有新的 Blocker、Major、correctness 或 data-race 问题。

## R-PROVIDER-001：M-PROVIDER/v0.1 首轮独立审查

- 范围：`internal/provider`、其测试，以及 provider 消费所需的 `internal/llm` 扩展；
  reviewer 未参与实现且未派生其他 agent。
- 验证：test、vet、build、race 重复、两组 fuzz 与固定上游 faux 行为核对均完成。
- 最终结论：`changes-required`；无 Blocker，两个 Major 与一个 Minor 见上表。
- 修订：候选修复已加入 typed failure/cause 贯穿、factory panic 边界、MaxInt-safe 分块及
  回归测试，等待定点复审。

## R-PROVIDER-002：M-PROVIDER/v0.1 定点复审

- 范围：R-PROVIDER-001 三项修订及其 `internal/llm` failure 贯穿；reviewer 全程只读且
  未派生其他 agent。
- 验证：test、vet、build、race、两包各 50 次定点 race 与单独 5 秒 fuzz 通过。
- 最终结论：`passed`；三个 finding 已关闭，没有新的 Blocker、Major 或 Minor。

## R-PROVIDER-003：M-PROVIDER/v0.2 整模块审查

- 范围：标准 `openai/openai-responses` text adapter 的 request/history、HTTP/SSE、reducer、
  usage、terminal、failure、cancellation 与 body ownership；reviewer 未参与实现且未派生
  其他 agent。
- 首轮结论：`changes-required`；无 Blocker，两个跨层 Major 见 `F-PROVIDER-004/005`。

## R-PROVIDER-004：M-PROVIDER/v0.2 定点复审

- 范围：R-PROVIDER-003 的两项修订及相邻回归；同一 reviewer 全程只读且未派生其他 agent。
- 验证：定点回归、provider test/race、SSE fuzz、全仓 test/race/vet/build 与 Windows、Linux、
  Plan 9 交叉编译通过。
- 最终结论：`passed`，Blocker 0 / Major 0 / Minor 0；真实 credential smoke、production
  assembler、tool/reasoning replay 仍属于后续里程碑。

## R-TOOL-001：M-TOOL/v0.1 首轮及修订复核

- 范围：`internal/tool` 的 Bash、runner、output、artifact 与跨平台 process tree；reviewer
  未参与实现且未派生其他 agent。
- 最终结论：`changes-required`。复核先后发现 cancellation settlement、Windows
  descendant cleanup、zero-value outcome、UTF-8/error 与 artifact fault 等缺口，以及
  Windows 386 Job Object 结构的 ABI padding 问题。

## R-TOOL-002：M-TOOL/v0.1 最终定点复审

- 范围：R-TOOL-001 修订后的完整模块与针对性回归；reviewer 未参与实现且未派生其他
  agent。
- 验证：全仓 test、vet、race、重复 settlement/process tests，Windows 386/amd64/arm64
  和 Plan 9 交叉编译通过；32/64 位 Job Object layout 有尺寸断言。
- 最终结论：`passed`，没有未关闭 Blocker 或 Major。真实 Windows Job Object lifecycle
  尚未在本机执行，保留为跨平台验证债务。

## R-TOOL-003：M-TOOL/v0.2 filesystem suite 首轮独立审查

- 范围：commit `3b6773d` 的 read/write/edit/grep/find/ls suite、registry/agent adapter、
  charter/ledger 与固定上游 filesystem tool 证据；reviewer 未参与实现。
- 结论：`changes-required`，0 Blocker / 5 Major / 2 Minor；finding 见
  `F-TOOL-001..007`。核心边界与纯 Go 方向成立，但 queue cancellation、symlink atomicity、
  patch correctness、ignore discovery、grep truncation、path normalization 与 cancellation
  contract 必须在复审前闭合。
- 修订：实现者已追加候选修复与故障/race/apply/cancel 回归，全部 finding 保持
  `fix-applied-awaiting-review`，不得在定点复审前标记 behavior/test 为完成。

## R-TOOL-004：M-TOOL/v0.2 filesystem suite 第二轮定点复审

- 范围：commit `c53f26a` 的 R-TOOL-003 候选修复、对应 filesystem suite 与 ledger；
  reviewer 只读复核 queue/write 及原七项 finding。
- 已关闭：`F-TOOL-003..007` 的 unified patch、ignore discovery、grep byte limit、NFD/AM-PM
  与 cancellation propagation 经复审确认关闭，不再扩展其实现。
- 结论：`changes-required`，0 Blocker / 2 Major / 0 Minor。`F-TOOL-001` 的每取消节点
  relay lifecycle 与 `F-TOOL-002` 缺 effective identity/ACL writability probe 尚未闭环。
- 当前修订：queue 改为取消调用同步等待 predecessor 后结算，增加批量取消后零残留
  node/key 的 deterministic seam/race oracle；existing target 在 prepare 和 rename 前均做
  identity-checked、无 truncation/append/write 的 `O_WRONLY` effective permission probe，并增加
  owner mode `0002` 与 content/mtime 稳定回归。两项仍为 `fix-applied-awaiting-review`，不声称
  本轮 review 已通过。

## R-TOOL-005：M-TOOL/v0.2 filesystem suite 最终定点复审

- 范围：commit `5f9ca71` 的 queue lifecycle 与 effective-writability 修订，以及
  `F-TOOL-001/002` 的相邻 symlink、mode、TOCTOU、取消和 race 回归；reviewer 只读复核。
- 结论：`passed`，0 Blocker / 0 Major / 0 Minor。取消节点同步等待 predecessor，settlement
  后 queue 无 node/key/goroutine 残留；existing target 在 prepare/commit 以原生
  `O_WRONLY` 做无写入权限探测并复核 identity/mode，owner `0002`、`0444`、symlink 和
  retarget 用例均通过。
- 验证：定点测试与 race 重复、全仓 test/vet/race/build、Linux/Windows test cross-compile
  和累计 diff check。Windows ACL runtime 未在当前 macOS 主机执行，保留平台验证边界。

## R-SESSION-001：M-SESSION/v0.1 首轮联合审查

- 范围：`internal/session` 完整 v3 JSONL aggregate、storage、resume/projection、并发与
  fault contract；reviewer 未参与实现且未派生其他 agent。
- 首轮结论：`changes-required`。原始参数 lexical resume、durable/in-memory 时间、
  alias-aware single writer 和 cancellable append 四项 Major 已修订；联合复审后又确认
  `F-SESSION-001..004`。
- 当前修订：JSON 语义等价使用 exact decimal normalization；Create 要求 parent 已存在；
  ISO timestamp 先验证可重开；Agent charter 明确 pre-write cancellation/post-write settle。
  全仓 test/race/vet/build、短时 fuzz 与多平台交叉编译已通过，等待最终复审。

## R-SESSION-002：M-SESSION/v0.1 最终定点复审

- 范围：R-SESSION-001 全部修订后的完整模块；reviewer 全程只读且未派生其他 agent。
- 核验：raw arguments 的上游 rewrite/reopen 与 exact-number 语义、preexisting parent
  durability contract、可重开 timestamp、alias writer 和 Append cancellation settlement。
- 最终结论：`passed`，0 Blocker、0 Major、0 Minor。真实掉电注入和真实 Windows runtime
  保留为平台验证债务，不阻塞 v0.1。

## R-SESSION-003：M-SESSION/v0.2-tree-branch 独立复审

- 范围：append-only forest、leaf select/reset、tree/path/context、active/external fork、branch
  extract、reopen 与 poison quarantine；reviewer 未参与实现。
- 闭环：`14d2302` 初版，`a2ade0b` 修复 active-source snapshot，`b900951` 禁止 poisoned
  export，`e2027d7` 以 synced uncertain tail 补实 fault evidence。
- 验证：全仓 test/vet/build/race、定点并发与 fault 回归、decoder/forest property fuzz，及
  Linux arm64、Windows amd64 交叉构建通过。
- 延期：branch-summary/compaction projection、label/custom/model/thinking entry 创建、v1/v2
  migration、multi-process writer 与真实掉电注入仍由后续里程碑重评。
- 最终结论：`passed`，Blocker 0 / Major 0 / Minor 0。

## R-SESSION-004：M-SESSION/v0.3-context-compaction 独立复审

- 范围：`421f5ba` 的 manual compaction、selected-path projection、v3 wire、并发/fault contract，
  以及 `bb25bb8` 对 `F-SESSION-005` 的 checked token arithmetic 修订；reviewer 未参与实现。
- 修订与验证：首轮 0 Blocker / 1 Major / 0 Minor；usage+trailing、message estimate 与 cut-prefix
  统一 fail-explicit 为 `ErrTokenEstimateOverflow`，并以 MaxUint64、Compact no-write 和 fuzz 关闭。
  全仓 test/vet/race/build、codec/forest/compaction/token fuzz、Linux/Windows/Darwin 交叉构建与
  diff check 通过。
- 明确延期：M-AGENT/M-APP 的真实 summarizer/manual surface integration、automatic threshold、
  provider retry/UI events，以及 branch-summary navigation/cache/invalidation；这些路径不得绕过
  `Session.Compact` 的 snapshot/commit gate。
- 最终结论：`passed`，Blocker 0 / Major 0 / Minor 0。

## R-SESSION-005：M-SESSION/v0.4-legacy-migration-recovery 独立复审

- 范围：v1/v2 `Open` migration、显式 trailing-partial recovery、atomic rewrite durability、
  alias-aware multi-process writer claim、Windows replacement 与相邻 application preservation；
  实现及闭环 commits 为 `7590f3d`、`730155c`、`67dc682`、`0e62c0d`、`a3be277`。
- 首轮 finding 由 `730155c` 关闭：hardlink rewrite 分裂、final symlink destination、rewrite 后
  新旧 inode claim，以及 temporary cleanup/publication fault 边界均改为 fail-closed 并补回归。
- Windows 两轮 finding 由 `67dc682`、`0e62c0d` 关闭：先补 share-delete identity handle 与
  write-through durability，再确认 `MoveFileExW` 不能满足 open-destination atomic replacement，
  最终只接受 `SetFileInformationByHandle(FileRenameInfoEx)` 的 replace + POSIX semantics。
- 末轮 finding 由 `a3be277` 关闭：publication 后 writer adoption 失败同时保留
  `ErrDurabilityUnknown` 与底层 typed cause；新 identity lock 必须用 locked-handle stat 对照锁前、
  锁后 path stat；Windows mandatory byte lock 移至 `1<<62`，不再阻塞 session data read/append。
- 最终核验：定点 migration/recovery/adoption/writer tests 重复 20 次，`go test ./...`、
  `go vet ./...`、`go build ./...`、`go test -race ./...`、legacy migration/recovery fuzz、Linux
  amd64/arm64 与 Windows 386/amd64/arm64 cross-compile、Windows 386/amd64 vet 及 diff check 通过。
- 平台债务：审查主机没有真实 Windows runtime 或 Wine，Windows-only read/append、alias、
  open-destination replacement、migration/recovery tests 尚未实机执行。`FileRenameInfoEx` 的最低
  product behavior 是 Windows 10 v1607 / Windows Server 2016；不支持该 information class 或
  POSIX flag 的系统/文件系统以 `ErrAtomicReplaceUnsupported` 在 publication 前 fail-closed，
  不使用较弱 fallback。
- 最终结论：`passed`，0 Blocker / 0 Major / 0 Minor；`B-SESSION-008` 与 `D-SESSION-001` 标为
  `ported`，`T-SESSION-008` 标为 `strengthened`。

## R-AGENT-001：M-AGENT/v0.1 完整模块联合审查

- 范围：`internal/agent` 的完整 single-tool loop、provider/tool/session 因果 barrier、
  busy/abort/settlement、fatal storage 和生产 Bash adapter；reviewer 未参与实现且未派生
  其他 agent。
- 核验：固定上游 Agent/AgentSession lifecycle 与 regression evidence、当前完整 worktree、
  terminal/Abort 和 tool/cancel 线性化、late update、stream Close、busy admission 及 storage
  successor barrier。全仓 test/race/vet/build 与多平台交叉构建由实现者在最终候选上通过。
- 修订：review 中发现的 Close panic 二次调用风险和 busy loser 触碰共享 clock 已修复并有
  回归；同步 observer 的 self-join 限制已写入 API 注释。
- 延后边界：durable ToolResult category/details 等待 M-BASE/M-SESSION 共同设计；真实
  provider tool schema 等待 adapter slice；mixed `length + toolCall` 由 `B/T-AGENT-009`
  等待 M-BASE 表达能力，不能用 text-only length 冒充。
- 最终结论：`passed`，0 Blocker，0 剩余 in-scope Major/Minor；关闭 `F-STAGE0-003`。

## R-AGENT-002：M-AGENT/v0.2 multi-tool queues 最终复审

- 范围：实现 `80d4094` 及修订 `84a8c93`、`7e587b9`、`7cbc1c5`；multiple tool calls、
  parallel/sequential override、completion/source-order split、queue/Continue、transform seam、
  Abort/late update/settlement 与对应 durable fault/race evidence。reviewer 未参与实现。
- 首轮结论：`changes-required`，0 Blocker / 3 Major / 3 Minor。修订关闭 Continue admission
  与 queue consume race、terminating batch 的统一 steering/follow-up stop path、sequential cancel
  的未执行 call 关联结算，以及 ClearAllQueues 原子性、batch 后 pending/phase 状态和不可达 v0.1
  控制器；`84a8c93` 使 v2 成为唯一控制流并补 deterministic interleaving/fault/race 回归。
- 第二轮结论：`changes-required`，0 Blocker / 1 Major / 1 Minor。`7e587b9` 以两阶段
  admission reservation 避免持 `Agent.mu` 调用 transcript storage port，并让 sequential/parallel
  pending tool state 成为 immutable multi-call snapshot；blocking/reentrant storage 与 state/event
  时点回归确认 slot、queue 和 settlement 不死锁、不误报。
- 第三轮结论：`changes-required`，0 Blocker / 1 Major / 0 Minor。`7cbc1c5` 将 queue drain
  改为 reserved prefix 与逐条 durable ack：Append 成功后才移除，首写/中途 fault 保留失败项和
  后继 FIFO，成功 prefix 不重复；concurrent enqueue/clear/Abort 均有明确线性化和 race oracle。
- 最终核验：定点 normal/error/cancel/fault/concurrency tests、agent suite 重复 20 次、
  `go test ./...`、`go vet ./...`、`go build ./...`、`go test -race ./...`、llm/session fuzz、
  Linux/Windows amd64 与 Darwin arm64 cross-build，以及累计 diff check 全部通过。
- 依赖与债务：M-TOOL/v0.2 filesystem 已由主线 `R-TOOL-005` 独立复审通过；Windows ACL
  runtime 仍是 M-TOOL 平台验证边界，不归入 Agent finding。Agent 无未关闭 in-scope finding；
  mixed `length + toolCall`、provider tool schema、retry/compaction 与 Harness 独有能力继续按
  charter 中既有 `B-AGENT-006/T-AGENT-007`、`B/T-AGENT-009` 及后续模块重评条件延期。
- 最终结论：`passed`，0 Blocker / 0 Major / 0 Minor；`B-AGENT-010..014` 标为 `ported`，
  `T-AGENT-010..013` 标为 `strengthened`，WF-003 通过。

## R-APP-001：M-APP/v0.1 与 WF-001 完整联合审查

- 范围：`internal/app`、`cmd/pi-go` 及 M-BASE/M-PROVIDER/M-TOOL/M-SESSION/M-AGENT
  组成的完整 headless tool workflow；reviewer 未参与实现且未派生其他 agent。
- 核验：固定上游 print/session/signal 行为、CLI admission、durable cwd、stdout/stderr/exit、
  signal settlement、invalid preservation、全新进程 restart/resume、test re-exec seam、
  production fail-closed 和跨平台构建。
- 修订：联合审查发现并关闭 leading-dash required value、session parent provisioning、
  SessionID 无副作用预检，以及重复 signal process evidence 缺口。
- 边界：真实 provider/auth、model selection 与默认 session path assembler 属下一里程碑；
  release binary 在此之前明确失败，不包含 deterministic fake 或 TypeScript fallback。
- 最终结论：`passed`，0 Blocker、0 Major、0 Minor；关闭 `F-STAGE0-010`，WF-001 通过。

## R-APP-002：M-APP/v0.2 production assembly 联合审查

- 范围：production CLI/model admission、OpenAI key/base URL source、只读 auth/models
  projection、release entry、默认/显式 session 与本地 HTTP/SSE WF-002；reviewer 未参与实现
  且未派生其他 agent。
- 首轮发现：selected OpenAI models 配置会静默忽略未知字段（Major）；预取 creation clock
  会污染 existing-session 首次 append（Minor）。
- 修订：selected provider 改为字段白名单、未知字段 secret-safe preflight fail；creation time
  只在 Create 分支重放，existing resume 使用原时钟，并加入对应无副作用/递增时钟回归。
- 验证：全仓 test/vet/race/build、配置 fuzz、本地 HTTP/SSE workflow，以及 Windows/Linux/
  Plan 9 build 和 app test compile 通过。
- 最终结论：`passed`，0 Blocker、0 Major、0 Minor；OAuth/auth 写入、command config value、
  完整 models/catalog、system prompt、tool wire 与真实 credential smoke 保留为后续里程碑。

## R-AUTH-001：M-AUTH/v0.1 联合审查与定点复审

- 范围：`internal/auth` 的 API-key storage、runtime overlay、config resolution、OpenAI production
  接入、相邻测试与 ledger；实现和闭环 commits 为 `4078ace`、`55fa34a`、`7a04595`，reviewer
  未参与实现。
- 修订：首轮 1 Blocker/2 Major/1 Minor 已关闭，包括 Windows private guarantee fail-closed、
  context-aware local serialization、稳定 B/T-AUTH ID 和统一 production resolver；定点复审新增的
  2 Major 由 Windows-only 相邻 suite、两个真实 contention process writers 与 failure-release
  证据关闭。
- 验证：全仓 test/vet/race/build、重复跨进程/锁取消定点测试、auth 与 production config fuzz、
  Linux/Windows cross-build、Windows auth/app vet 和 test-binary compile 均通过；PE symbol 核对确认
  Windows-only tests 已编入。
- 平台边界：审查主机没有 Windows runtime 或 Wine，Windows-only tests 尚未实机执行；v0.1
  不实现 DACL-backed persistent auth，在 Windows 对 existing/set/delete 保持明确 fail-closed，
  不能把 cross-compile 写成 DACL 或实机验证完成。
- 明确延期：OAuth login/refresh、command-backed config execution、Windows DACL persistence；
  未知 stale lock 不自动回收仍是明确 contract，不由本里程碑静默改变。
- 最终结论：`passed`，Blocker 0 / Major 0 / Minor 0。
