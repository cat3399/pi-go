# 上游范围分类

本表区分固定上游仓库中的产品行为、独立 library 能力、测试证据和实现细节。文件在
pi 仓库中存在不等于它必须进入首个 pi-go workflow；反过来，后移也不等于永久排除。

| 分类 | 固定上游区域 | 迁移处理 |
| --- | --- | --- |
| 当前 standalone 产品主路径 | `packages/coding-agent/src`；`packages/agent/src/agent.ts`、`agent-loop.ts`；`packages/ai/src`；`packages/tui/src` | 作为完整产品范围，按 domain/slice 迁移；先 headless core，后 interactive/TUI |
| 当前 product entry | coding-agent package bin、`src/cli.ts`、`src/main.ts`、print/interactive/RPC modes | CLI/print 是早期 workflow；interactive/TUI 后续；RPC 重新评估 remote/product 语义 |
| 上游独立 library product | `packages/agent/src/harness` 及其 JSONL/session/resource/tool 能力 | 不是当前 executable caller；提取共享 invariant，独有能力 deferred，不能复制第二套 runtime |
| 上游次级 product capability | `packages/protocol`、`client`、`server`、`storage/sqlite-node` | 属于完整上游范围，但按产品依赖在后续 Remote/Session milestone 重新决定边界，不作为 early core 前置 |
| Extension 产品能力 | coding-agent extension loader/types/hooks、extension examples | standalone core 稳定后从真实能力提炼；TypeScript source compatibility、module loading 和巨大 type surface 不直接迁移 |
| Test evidence | 各 package `test/`，尤其 coding-agent `test/suite/` 和 deterministic faux harness | 测试意图进入 TESTS ledger；不逐行翻译，不让真实 credential/e2e 成为默认 suite |
| Fixture/data evidence | session/model/provider test data、inline scripted response、历史 JSONL | 按 behavior 选择最小脱敏 fixture；大型历史数据保留给 migration/compaction，不复制无关 fixture |
| Example/scratch | package examples、`test/scratch`、演示脚本 | 先判断是否承载唯一产品语义；通常是使用证据或 test aid，不作为完成条件或 public API 承诺 |
| Eval tooling | `packages/evals` | 不在首个 standalone runtime；完整上游阶段再判断哪些评测是产品质量工具、哪些不适用于 Go 产品 |
| Generated/catalog artifact | `models.generated.ts`、provider model shards、image model data | 冻结产物和 validator 是证据；生成输入缺口见 PROVIDERS，不能按生成行数迁移 |
| Build/release/runtime helper | package scripts、发布入口辅助、Node/npm 配置、预编译 binary | 若只验证 TypeScript/packaging，则 test 为 not-applicable；跨平台用户行为另建 Go build/release slice |
| Native/platform adapter | TUI native C、shell/process/platform utility | 行数小但属于风险边界；在 TUI/tool 对应 slice 用 Go/platform test 重新实现，不机械翻译 |
| External downstream | pi-web 参考 commit 与第三方 plugin | 不是主要上游规范；standalone/完整迁移后重新取证并由 adapter/消费者承担具体兼容 |

## 分类规则

- 同一文件可以同时包含产品行为和实现细节，分类粒度最终落到 behavior/test intent，
  不能用整目录标签替代阅读。
- Example 或 test 如果是某行为唯一可观察证据，应关联到 ledger；它仍不会因此成为
  production dependency。
- AgentHarness、protocol/server/client 等“后移”项目必须有重评条件，不得用阶段顺序
  变相永久删除上游产品能力。
- Extension types、generated model list 或 eval 行数不能用来计算迁移完成率；完成只由
  behavior、test disposition 和 workflow 决定。
- 新发现的区域若不能确定分类，先记 `unclassified` 并停止依赖它的实现，不凭目录名
  猜测为 product 或 implementation-only。
