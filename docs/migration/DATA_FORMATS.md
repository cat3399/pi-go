# Durable data inventory

本清单记录固定上游中可能影响用户升级、恢复或 credential 安全的数据。`classified`
表示格式与来源已定位，不表示 reader/writer 已在 Go 中实现。

| ID | 数据 | 固定上游证据 | 兼容要求 | 当前状态 |
| --- | --- | --- | --- | --- |
| `D-SESSION-001` | coding-agent session JSONL v1-v3 | `packages/coding-agent/docs/session-format.md`；`src/core/session-manager.ts` | v1/v2 Open migration → strict v3 rewrite；unknown 保留；corrupt/partial 不自动破坏 | `ported`（R-SESSION-005） |
| `D-SESSION-002` | AgentHarness JSONL v3 | `packages/agent/src/harness/session/jsonl-store.ts` | 只提取 strict parse/storage invariant；不能假定与 D-SESSION-001 wire 等价 | `classified` |
| `D-SESSION-003` | AgentHarness SQLite | `packages/storage/sqlite-node/src/sqlite/` 和 migrations | 后续 storage backend；不阻塞 standalone JSONL | `deferred` |
| `D-SESSION-004` | legacy `~/.pi/agent/*.jsonl` root location | `packages/coding-agent/src/migrations.ts::migrateSessionsFromAgentRoot` | 校验 header/cwd 与 destination collision；迁移成功前不覆盖或丢失 source | `deferred` |
| `D-SESSION-005` | v3 `compaction` / `branch_summary` session entries | coding `session-manager.ts`；coding/Harness compaction modules | compaction envelope/parent/first-kept/usage 可严格读取写入；branch summary raw-preserved but not projected until navigation/cache contract | `compaction implemented; branch_summary deferred` |
| `D-SETTINGS-001` | global/project `settings.json` | `packages/coding-agent/src/core/settings-manager.ts` | source precedence、project trust、unknown setting preservation | `classified` |
| `D-TRUST-001` | `~/.pi/agent/trust.json` project trust decisions | `packages/coding-agent/src/core/trust-manager.ts::ProjectTrustStore` | 安全关键；path normalization、nearest ancestor、lock/merge、unknown/malformed preservation | `classified` |
| `D-KEYBINDING-001` | `~/.pi/agent/keybindings.json` | `packages/coding-agent/src/core/keybindings.ts`；`packages/coding-agent/src/migrations.ts::migrateKeybindingsConfigFile` | TUI 前实现；unknown/malformed 不覆盖，alias migration 可重复 | `deferred` |
| `D-AUTH-001` | `auth.json` provider credential map | `packages/coding-agent/src/core/auth-storage.ts`；`packages/ai/src/auth/types.ts` | mode 0600、lock/merge、malformed 不覆盖、secret 不进入 log | `ported-api-key-v0.1-unix`；Windows fail-closed；OAuth deferred |
| `D-AUTH-002` | legacy `oauth.json` 与 settings API keys | `packages/coding-agent/src/migrations.ts` | 后续 one-way migration，保留原文件直至成功 | `deferred` |
| `D-MODEL-001` | user `models.json` | `packages/coding-agent/src/core/model-config.ts` | comments/schema、provider overrides、错误诊断和 reload | `classified` |
| `D-MODEL-002` | dynamic `models-store.json` | `packages/coding-agent/src/core/models-store.ts`；`packages/ai/src/models-store.ts` | provider-scoped merge、etag/checkedAt、malformed 不覆盖 | `classified` |
| `D-RESOURCE-001` | global/project `prompts/` 与 legacy `commands/` | `packages/coding-agent/src/migrations.ts::migrateCommandsToPrompts` | 资源清单先保留；collision 不覆盖，迁移成功前不丢 source | `deferred` |
| `D-RESOURCE-002` | global/project extensions、skills、themes 与 package resource dirs | `packages/coding-agent/src/core/resource-loader.ts`；`packages/coding-agent/src/migrations.ts` | 后续资源模块逐类取证；当前不得把未知 user resource 当临时文件清理 | `deferred` |
| `D-CATALOG-001` | built-in generated model catalog | `packages/ai/src/models.generated.ts`；`packages/ai/scripts/generate-models.ts` | 冻结产物可读；原始 data/manifest 缺失，不能 live regenerate 冒充基线 | `baseline-artifact-gap` |

## D-SESSION-001：首个兼容子集

M-SESSION/v0.1 只写 coding-agent v3 header 和线性 `message` entries，足以保存 WF-001
的 `user, assistant(toolUse), toolResult, assistant(final)`。Header/entry/message/content
中的 unknown JSON 值都要保留；domain context 只投影已知 message。

读写策略与故障 fixture 见 [modules/SESSION_STORAGE.md](modules/SESSION_STORAGE.md)。特别是：

- 不复制 coding-agent 的 malformed-line silent skip；
- 不把 Harness `leaf` entry 当作 coding-agent current-leaf 语义；
- future version、partial final record、duplicate ID 和 broken parent 均拒绝写回；
- append 首次写入后的 failure 不能先推进内存 leaf，且 writer 必须 poison；磁盘结果可能
  不确定，不能承诺原 bytes 不变；
- v0.1 明确 single-process writer，multi-process lock 后续单独设计。

M-SESSION/v0.2 仍只写同一 v3 header/entry wire，但 writer 可以在显式 `ResetLeaf` 后增加
另一个 `parentId:null` root，或在 `SelectLeaf(id)` 后向非物理 tail 加 child。选择本身不写
record；重开时 physical last entry 是 selected leaf。branch extract 创建独立 JSONL，保留
selected path 的原始 entry bytes，并写 new header 的 `parentSession`；fork 保留 source forest
的所有 entry bytes。活跃 aggregate 通过 `Session.Fork` 在 append gate 下 snapshot；只有未由
当前进程持有 writer claim 的外部文件才走 `ForkFrom(path)` 的 strict Open。目标 create
必须 no-replace/atomic，任一失败不得改 source。若活跃 source 已因 append
commit-unknown poisoned，Fork/Extract 必须先返回 `ErrPoisoned`，不能用可能落后磁盘的
内存 snapshot 创建目标；调用者仍须 close/reopen/reconcile。

M-SESSION/v0.4 将同一 inventory 的 v1/v2 作为 `Open` 的 legacy 输入：v1 物理链与
compaction index、v2 tree envelope 与 hook rename 转成 strict v3，同时保留 unknown raw JSON。
普通 Open 不修改 corrupt 或 trailing-partial v3；后者只能经显式 `RecoverTrailingPartial`
先建立 no-clobber backup，再发布已严格校验的完整 prefix。migration/recovery 的完整 rewrite、
writer-lock 与平台债务 contract 见 `R-SESSION-005` 和 SESSION_STORAGE charter。

## D-SETTINGS-001

上游同时有 global agent settings 和 project `.pi/settings.json`，project source 受 trust
控制并覆盖 global。格式包含 product service、TUI、package/resource、retry、session
path 等多类设置，不能在 Go 中复制成一个巨大 service。

迁移按实际消费者切片：首个 workflow 暂不依赖 settings；Bash timeout、session path、
model/auth 等行为出现时只读取所需字段。任何写回必须保留未知设置，malformed reload
保留最后有效 snapshot 并报告诊断。

## D-TRUST-001 / D-KEYBINDING-001

`trust.json` 不是普通偏好：它决定 project settings/resource 是否可加载。任何 settings、
prompt、extension、skill 或 project-local config slice 在读取这些资源前，都必须先实现或
显式替代该 trust decision；缺文件不等于 trusted。写回要复用 path normalization、最近
祖先 decision 和 lock/merge 语义，malformed/unknown data 不得被默认值覆盖。

`keybindings.json` 等到 interactive/TUI consumer 出现再实现。上游包含 key alias 的
one-time migration；pi-go 必须用 fixture 证明 migration 可重复且 collision/unknown action
不会丢失，不能在启动时无诊断重写用户文件。

## D-AUTH-001

`auth.json` 是 provider ID 到 type-tagged API-key/OAuth credential 的 JSON object。上游
使用 file lock、mode 0600 和 read-modify-write 以保留其他 provider 的并发编辑；
malformed 文件不得被新 credential 覆盖。

Go auth storage 必须进一步使用 secret-safe error/log、atomic replacement 和明确的
credential source precedence。Deterministic fake 不读取该文件；真实 provider/auth
里程碑再建立 fixture，fixture 只含不可用测试值。

M-AUTH/v0.1 实现 OpenAI 及其他 provider API-key entry 的 read/write/delete runtime service；unknown
provider entry 保留，malformed 或不支持的已选 credential 明确失败且不得回退环境变量。OAuth/login/
refresh 和 legacy migration 仍未迁移，故 D-AUTH-001 不代表完整 credential-format migration。Windows
尚无可靠 DACL admission/creation，persistent read/write/delete fail-closed；missing file 不阻塞 runtime、
models.json configured key 或 ambient environment source。

## D-MODEL-001 / D-MODEL-002

`models.json` 是用户配置，dynamic store 是 provider catalog cache；二者职责不同，
不能合成一个 schema。前者允许 provider/model override 与 compat metadata，后者保存
provider-scoped model list、Last-Modified、checkedAt 和 opaque ETag。

首个真实 adapter 使用手写最小 model fixture，不依赖完整 catalog。Catalog baseline
缺口和重评条件见 [PROVIDERS.md](PROVIDERS.md)。

M-APP/v0.2 只读投影 `providers.openai.apiKey/baseUrl`，并接受无请求语义的 `name` 与固定
`openai-responses` API 标记。选中 OpenAI provider 的其余字段明确失败；这不等于完整迁移
D-MODEL-001。

## Legacy location 与 user resource

旧 root session 和 `commands/ -> prompts/` 是兼容迁移，不是普通目录整理。实现时先
解析/验证 source，再检查 destination collision，成功 durable 后才允许把 source 标记为
已迁移；任何失败都保留 source。Extensions、skills、themes、prompts 和 package resources
先作为 user resource inventory 保留，等对应产品 slice 再分别建立格式与 trust contract。
