# M-MODEL：settings 与 model catalog runtime charter

状态：`implemented; independent review pending`

## 职责与边界

`internal/model` 拥有严格的 `models.json`（JSONC）与 global `settings.json`
快照、case-fold canonical provider/model overlay、最小 builtin OpenAI baseline、精确 model
解析、enabled-model 顺序、provider-scoped `models-store.json` 与原子 reload。
它的 snapshot 为值拷贝；失败 reload 不发布半成品。全局 settings 写入在进程内 mutex
和跨进程 lock 下重读、合并并原子 publish，未知 root fields 保留。mutation 要求 durable
parent 已由 application create phase 建立；模块只同步临时文件、rename 后的 leaf parent，
不会以未同步祖先的 `MkdirAll` 冒充 durable success。

它不拥有 credential resolution（`internal/auth`）、provider request adapter、session/tool、
project trust decision、remote refresh、fuzzy selection 或 model cycling。项目
`.pi/settings.json` 只有 `ProjectTrusted: true` 时才读取；production v0.1 明确传 false。

## 上游证据

- `packages/coding-agent/src/core/{model-config,model-runtime,model-resolver,model-registry,models-store,provider-composer,settings-manager,defaults}.ts`
- `packages/coding-agent/test/{model-resolver,models-store,settings-manager,model-registry}.test.ts`
- regressions `3217`, `6949`, `6999`, `3616`, `2753`。

固定基线为 `a116523434806910336b9de3e38a41aa5860030b`。API dialect 是 model
metadata 的显式字段；production 仅装配 `openai/openai-responses`，其余组合在 network/session
副作用前明确失败。

## v0.1 slices

| ID | 行为 | 状态 |
| --- | --- | --- |
| B-MODEL-001 | JSON/JSONC strict admission、duplicate 与 secret-safe diagnostic | ported/strengthened |
| B-MODEL-002 | provider/model overlay、custom model 与最小 OpenAI baseline | ported/strengthened |
| B-MODEL-003 | CLI/provider-prefixed、settings default、ordered enabled scope | ported (exact only) |
| B-MODEL-004 | transactional reload、snapshot clone、settings/store atomic persistence | strengthened |
| B-MODEL-005 | production preflight only supported provider/API and selected-route unsupported/unknown fields | in-progress (independent review pending) |

## 明确债务

固定 commit 的 generated catalog 引用的 data JSON 和 manifest 不在 tree 中，不能重建或
声称已迁移完整 catalog。remote refresh 需可验证 manifest/source、per-provider HTTP contract
及 auth policy 后才可开始。fuzzy selection、model cycling 和正式 project trust 决策同样延后，
需要其各自的 interactive/trust workflow 和 regression suite。
