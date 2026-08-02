# Provider matrix

本文记录固定上游的 text provider、API dialect、认证来源与 catalog 类型。矩阵用于按
dialect/行为族规划 adapter，不表示每个 provider 都应有一个同名 Go package。

状态：provider/API/auth 映射已 `classified`；精确 model catalog 存在下述 baseline
artifact gap。

## API dialect

固定基线定义十个已知 text API dialect：

- `openai-completions`
- `mistral-conversations`
- `openai-responses`
- `azure-openai-responses`
- `openai-codex-responses`
- `anthropic-messages`
- `bedrock-converse-stream`
- `google-generative-ai`
- `google-vertex`
- `pi-messages`

同一个 provider 可以按 model 使用多个 dialect。Provider 名称是 auth、catalog 和 policy
边界；wire conversion/stream parser 以 API dialect 为主要复用边界。

## Text provider

普通 provider 的注册证据位于 `packages/ai/src/providers/<id>.ts`；静态 catalog shard
位于同目录 `<id>.models.ts`。

| Provider | API dialect | 认证来源摘要 | Catalog | Adapter 分类 |
| --- | --- | --- | --- | --- |
| `amazon-bedrock` | bedrock-converse-stream | stored bearer；AWS bearer/profile/access key/ambient chain | static | 专用 Bedrock |
| `ant-ling` | openai-completions | `ANT_LING_API_KEY` | static | 薄 registration |
| `anthropic` | anthropic-messages | stored key/token、environment、OAuth | static | 专用 Anthropic |
| `azure-openai-responses` | azure-openai-responses | Azure API key | static | 专用 Azure Responses |
| `cerebras` | openai-completions | API key | static | 薄 registration |
| `cloudflare-ai-gateway` | anthropic-messages、openai-completions、openai-responses | API key + required account ID + gateway ID | static | mixed dialect + endpoint wrapper |
| `cloudflare-workers-ai` | openai-completions | API key + account | static | shared adapter + endpoint wrapper |
| `deepseek` | openai-completions | API key | static | 薄 registration |
| `fireworks` | anthropic-messages、openai-completions | API key | static | mixed dialect |
| `github-copilot` | anthropic-messages、openai-completions、openai-responses | token 或 OAuth | static + credential filter | mixed dialect |
| `google` | google-generative-ai | Gemini API key | static | 专用 Google |
| `google-vertex` | google-vertex | API key、ADC/service account、project/location | static | 专用 Vertex |
| `groq` | openai-completions | API key | static | 薄 registration |
| `huggingface` | openai-completions | token | static | 薄 registration |
| `kimi-coding` | anthropic-messages | API key 或 OAuth | static | 薄 registration |
| `minimax` | anthropic-messages | API key | static | 薄 registration |
| `minimax-cn` | anthropic-messages | API key | static | 薄 registration |
| `mistral` | mistral-conversations | API key | static | 专用 Mistral |
| `moonshotai` | openai-completions | API key | static | 薄 registration |
| `moonshotai-cn` | openai-completions | API key | static | 薄 registration |
| `nvidia` | openai-completions | API key | static | 薄 registration + NIM metadata |
| `openai` | openai-responses | API key | static | 标准 Responses |
| `openai-codex` | openai-codex-responses | OAuth | static | 专用 Codex Responses |
| `opencode` | anthropic-messages、google-generative-ai、openai-completions、openai-responses | API key | static | four-dialect registration |
| `opencode-go` | anthropic-messages、openai-completions、openai-responses | API key | static | mixed dialect |
| `openrouter` | openai-completions | API key 或 OAuth | static | shared completions |
| `qwen-token-plan` | openai-completions | API key | static | 薄 registration |
| `qwen-token-plan-cn` | openai-completions | API key | static | 薄 registration |
| `radius` | pi-messages | API key 或 OAuth | dynamic config/cache | 专用 dynamic provider |
| `together` | openai-completions | API key | static | 薄 registration |
| `vercel-ai-gateway` | anthropic-messages | API key | static | shared Anthropic |
| `xai` | openai-completions、openai-responses | API key 或 OAuth | static | mixed dialect |
| `xiaomi` | openai-completions | API key | static | 薄 registration |
| `xiaomi-token-plan-cn` | openai-completions | API key | static | 薄 registration |
| `xiaomi-token-plan-ams` | openai-completions | API key | static | 薄 registration |
| `xiaomi-token-plan-sgp` | openai-completions | API key | static | 薄 registration |
| `zai` | openai-completions | API key | static | 薄 registration |
| `zai-coding-cn` | openai-completions | API key | static | 薄 registration |

Image generation 是独立 runtime：固定基线只有 OpenRouter provider 使用
`openrouter-images`。它不进入首个 text/tool workflow。

## 共同 runtime 行为

主要证据是 `packages/ai/src/models.ts`、`packages/ai/test/providers.test.ts` 和
`packages/ai/test/models-runtime.test.ts`：

- mixed provider 按 `model.api` dispatch；没有实现的 dialect 产生 error stream；
- stored credential 的 type 先选择 auth handler；若该 type 没有匹配 handler，不得跨到
  另一个 ambient handler。选中的 `ApiKeyAuth` 可按 provider policy 对缺失字段逐项合并
  ambient 值，例如 Cloudflare account/gateway、Bedrock profile/region 与 Vertex
  project/location；stored object 不是“一项存在就屏蔽所有 ambient 字段”；
- 显式 request option 覆盖 resolved auth field，header 合并大小写不敏感；
- credential metadata 枚举不得暴露 secret；
- dynamic provider refresh 支持 cache restore、in-flight dedupe、abort 和逐 provider error；
- provider model list 失败不能使整个 catalog listing 崩溃，但精确 provider error 仍可诊断。

这些共性按 behavior slice 迁移。OAuth、dynamic catalog 与所有 auth source 不进入
deterministic fake 里程碑。

## Model catalog baseline artifact gap

固定 commit 的 Git tree 包含：

- `packages/ai/src/models.generated.ts`；
- 37 个 `packages/ai/src/providers/*.models.ts`；
- `packages/ai/scripts/generate-models.ts`。

但 tree 不包含这些 shard 引用的 `packages/ai/src/providers/data/*.json` 和
`.manifest.json`。生成器还会实时请求 models.dev、OpenRouter、NVIDIA 和 Vercel，再
叠加 curated override。因此：

- 当前 `models.generated.ts` 是固定 commit 可读取的冻结产物；
- 具体 model entry 的原始 catalog、manifest/hash 和完整生成输入不可由该 commit
  单独重建；
- 现在重跑 live generator 得到的是新外部状态，不能冒充 0.83.0 fixture；
- 取得对应发布包中的 manifest/hash 或上游补齐的生成 artifact 后，重新评估该 gap。

在 gap 关闭前，可以迁移生成数据的 schema/hash/provider-api-id consistency validator
意图，证据为 `packages/ai/scripts/model-data.ts` 与
`packages/ai/test/model-data-validation.test.ts`；不能声称精确 catalog 已可重复生成。

这个 gap 不阻塞 deterministic provider、Agent runtime 或首个 headless workflow，也不
阻塞使用最小手写 model fixture 验证一个真实 dialect。

## 首个真实 dialect

Fake 与 agent 闭环稳定后，首个真实 adapter 选择标准 `openai-responses` 的 text
request、client/error wrapper、SSE streaming 与 terminal handling。源码证据包括
`packages/ai/src/api/openai-responses.ts` 和
`packages/ai/src/api/openai-responses-shared.ts`。先由本地 HTTP/SSE fixture 验证
request/stream/error 与 custom fetch transport，再运行显式启用的
`packages/ai/test/stream.test.ts` 中 `OpenAI Responses Provider (gpt-5.4)` 的
`should complete basic text generation`、`should handle streaming` smoke intent。

`packages/ai/test/openai-responses-partial-json-cleanup.test.ts` 验证的是 function-call
arguments cleanup，不属于 text-only slice，保留到 `B-PROVIDER-002` 与真实 tool-call
adapter milestone。Tool、thinking、cache 和完整 compat matrix 分别进入后续 slice。

M-AGENT/v0.3 在该 adapter 上新增并由 `R-AGENT-003` 复审通过的 adjacent failure-policy seam：OpenAI
HTTP 400 优先按 allowlisted type/code 归一 secret-safe `contextOverflow`；message-only fallback
只接受明确 input/prompt-context 短语，并排除 output/completion/max-output 和 parameter errors，
因此模糊 context wording 与普通 400 不进入 compact/retry。`Retry-After` 支持 unsigned ASCII
`1*DIGIT` delta-seconds 与 future HTTP-date；`+17`、`-0`、past/malformed 忽略，shared controller
的零 `MaxRetryAfter` 为 60s。adapter 只提供 typed metadata/observer，Agent 才拥有一次性
`Session.Compact` admission 和 summarization-scoped lifecycle，Summarizer/turn 才拥有各自有限
attempt budget；provider 不反向依赖 Agent。这不改变 R-PROVIDER-004 对既有 text adapter 的
review 范围。

最终 core integration 已把 `c8d1d1c`、rich replay 与 context/retry 合并，并由联合 local
HTTP/SSE oracle 固定 tools/parallel/rich replay 与 retry 重建。CLI tuning surface 与真实
credential smoke 仍 deferred；历史 review 范围不因该 integration 扩张。

选择该入口是为了先验证一个专用、边界清楚的 adapter；它不改变完整迁移需要覆盖
其他九个 dialect 和上述 provider policy 的目标。
