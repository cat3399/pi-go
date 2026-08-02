# M-RESOURCE：可信 prompt assets charter

状态：`implemented-awaiting-independent-review`（`M-RESOURCE/v0.1-trusted-prompt-assets`）

## 负责

- `internal/resource` 单独拥有 global agent-dir 与经 trust 决策后的 cwd/project 指令、`SYSTEM.md`、append prompt、prompt template 与 `SKILL.md` 的发现、验证、不可变 snapshot、reload 和 system-prompt assembly；
- durable `trust.json` 的 strict parse、父目录继承、unknown decision 保留、Unix private admission、atomic replacement、跨进程 lock 与 cancellation；Windows 无法提供等价 private-file guarantee 时读既有文件与 mutation 均 fail-closed；
- resource 先于 production session/network/auth 之后的 model request 装配；无记录 trust 一律只使用 global/default 资源，绝不启动 UI、浏览器或自动信任。

## 明确不负责

- extension/package 动态加载、remote resources、interactive trust selector、settings schema 和 legacy `commands -> prompts` migration；这些均仍 deferred。
- Go 不调用、启动或 fallback 到 TypeScript。

## 关键 contract

- 未明确 trusted 的 cwd 只作 lexical absolute key lookup，不 `stat`/canonicalize/read/parse/discover；因此 project 文件（包括 dangling/loop symlink）不能改变 global prompt、错误或 resource-loader 时序。明确命中后才 canonicalize，并拒绝逃离该 physical trust anchor 的链接。
- global 先加载，trusted project 覆盖同名 template/skill；collision 有 deterministic diagnostic。所有目录顺序稳定，symlink、invalid UTF-8、oversize、malformed frontmatter/skill 都明确失败，绝不拼入 prompt。
- reload 以锁内 generation reservation、锁外候选构造、最终 trust recheck + generation 条件发布；旧 trusted build 不能覆盖更新的 untrusted reload。失败保留最后健康 snapshot，首次失败返回 unavailable；返回 slices 不可反向改写。
- trust JSON 原样保留 `boolean`、`null` 与 future raw JSON（含超大 number）；`null` 继续查父级，future value 是未授权 stop point，不能降级继承父级 `true`。serialized bytes 在 rename 前受总量限制；rename 后 directory sync 失败以 `ErrCommitUnknown` 返回，调用者必须 reopen/reconcile。
- `SYSTEM.md` 取代默认主体；append/instructions/templates/visible skills 与当前 cwd、仅真实可用 tool 一起有边界化 assembly，超过总上限 fail-closed。`disable-model-invocation` skill 不进入模型 prompt；已 admitted template 在 `-p` 进入 session/provider 前展开。

## 上游证据与 disposition

- `packages/coding-agent/src/core/{resource-loader,system-prompt,prompt-templates,skills,project-trust,trust-manager,source-info}.ts`，基线 `a116523434806910336b9de3e38a41aa5860030b`；
- `test/{resource-loader,system-prompt,prompt-templates,skills,trust-manager}.test.ts` 与 regressions `2753`、`2781`：核心 discovery/collision/template/skill/trust 迁移并以 Go fail-closed、race/fuzz/fault 加强；extension/package、interactive selector、settings hot reload 和 remote 资源 deferred。

## v0.1 slices

| ID | 行为 | 状态 |
| --- | --- | --- |
| `B-RESOURCE-001` | trust-first global/project discovery 与 untrusted non-probe | `implemented-awaiting-independent-review` |
| `B-RESOURCE-002` | strict private durable trust decision | `implemented-awaiting-independent-review` |
| `B-RESOURCE-003` | instruction/template/skill validation, collision and expansion | `implemented-awaiting-independent-review` |
| `B-RESOURCE-004` | immutable snapshot/reload and bounded system prompt | `implemented-awaiting-independent-review` |
| `B-APP-009` | production prompt assembly before session/network | `implemented-awaiting-independent-review` |
