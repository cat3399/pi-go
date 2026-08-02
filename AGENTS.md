# pi-go 协作规则

## 开始前

修改核心代码前读取 `README.md`、`docs/ARCHITECTURE.md` 和 `docs/STATUS.md`。制定近期
任务时再读取 `docs/ROADMAP.md`，不需要读取历史迁移台账。

事实优先级如下：

1. 原版 pi 的实际调用链、行为和测试；
2. pi-go 当前实现、production assembly 和测试；
3. 本仓库文档中的阶段总结。

如果三者冲突，先调查实现，不得为了符合文档维持错误架构；确认后同步更新文档。

## 核心方向

- 产品 runtime 只使用 Go，不启动、代理或 fallback 到 TypeScript pi。
- 首要任务是形成 `AgentLoop -> AgentSession -> Runtime/RPC` 的完整核心。
- Model、Message、Provider 和 AgentSession 默认移植原版已经工作的语义边界，不另造
  平行抽象。Go 化调整必须有具体的类型安全、并发或资源生命周期理由。
- 图片和富 tool result 属于 Agent 核心消息链路，不是以后再补的 UI 功能。
- pi-web 是核心完成后的重要验收目标；优先复用原版 RPC 行为，让 pi-web 修改启动和
  transport，不在 core 中加入页面专用状态。
- 完整旧 session 兼容、TUI、plugin 和 extension 可以暂缓，但当前设计不得主动阻塞
  这些能力。
- 初期 API 保持 internal，只有真实调用者稳定后才冻结 public boundary。

## 文档纪律

- 不为每个 package、小功能或 review 建立 charter、ledger 和流水账。
- `docs/STATUS.md` 只记录影响整体判断的事实；实现状态变化时直接更新。
- 重要行为由代码、测试和端到端 scenario 证明，不能用文档状态宣称完成。
- 上游 commit 变化时更新 `docs/STATUS.md` 中的参考版本即可；只有行为差异需要说明。

## 修改与测试

- 先沿调用链调查现状，再修改；明显与预期不符时不要猜测。
- 默认测试不得依赖 network、真实 credential 或 TypeScript runtime。
- Go 代码变更至少运行 gofmt、`go test ./...`、`go vet ./...` 和 `go build ./...`。
- 涉及 agent、streaming、session、tool 并发或取消时运行相关 race test。
- 端到端行为优先于孤立 package 的局部完成度。

## 工作区安全

- 不相关修改属于用户，保留并忽略，绝不使用 Git 回退命令。
- 永远不要删除文件；确实需要移出仓库时移动到 `/tmp` 并说明位置。
- 临时文件放在 `/tmp`，不要污染仓库。
- 搜索必须针对工作区或明确目录，不做全局 filesystem 搜索。
- 委派子 agent 时必须明确禁止其继续派生子 agent。
