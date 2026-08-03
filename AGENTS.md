# pi-go 协作规则

## 开始前

修改核心代码前完整读取 `README.md`、`docs/ARCHITECTURE.md`、`docs/ROADMAP.md` 和
`docs/STATUS.md`。仓库不维护历史迁移台账；如果发现旧说明或代码注释与当前契约冲突，
在同一任务中清理。

事实优先级如下：

1. 原版 pi 的实际调用链、类型、行为和测试；
2. 可在两种实现间复用的 fixture 与端到端 scenario；
3. pi-go 当前实现、production assembly 和测试；
4. 本仓库文档中的阶段总结。

当前测试也可能固化了旧迁移行为。发生冲突时必须先取得原版源码和行为证据，再决定修改
实现还是测试；不得只因为旧测试通过就保留错误架构。确认后同步更新相关文档。

## 当前核心目标

- 产品 runtime 只使用 Go，不启动、代理或 fallback 到 TypeScript pi。
- 首期调用链是 `Runtime -> AgentSession -> Agent -> AgentLoop`，SessionManager 与
  Session Store 保持独立职责。
- Model、AgentMessage、ToolResult、session entry、event 与 hook 默认完整移植原版语义，
  不另造缩水的平行抽象。
- Go 化调整只改变语言表示、并发、错误和资源生命周期；不得降低能力、删除信息、合并
  原版独立层次或改变可观察行为。
- 图片、rich tool result、custom/bash/compaction/branch message、usage/cost、queue、retry、
  compaction 和 session tree 都是 Agent 核心，不是 RPC 或 UI 功能。
- 当前完成点是可长期驱动、通过整体行为验收的 in-process Runtime，不是 CLI demo、RPC
  server 或 pi-web 页面。

## 明确后置项

- JSONL RPC 的 framing 和进程控制；
- Provider adapter 与 model catalog 的广度；
- extension/plugin loader 和外部桥接；
- pi-web transport 切换；
- TUI 与其他 surface。

后置的是实现，不是它们依赖的核心契约。完整 Model 结构、通用 Provider boundary、传输
无关 event/state、extension-neutral hook 和 custom session data 必须在首期正确形成。

## 重构策略

- 当前所有 Go API 保持 internal，旧 package、类型、文件布局和函数签名没有兼容承诺。
- 不以最小 diff 为目标。现有实现符合目标就复用，不符合就移动、拆分或重写。
- 不为赶 RPC/pi-web 在 Runtime 外建立第二套 state，不在 core 中加入页面专用数据。
- 先沿原版与 Go 的完整调用链调查所有权，再改代码；不要从某个孤立类型猜整体架构。
- 新 Provider 不应要求修改 AgentLoop；新 surface 不应要求修改 session/message 基本语义。

## 阶段与验收

按 `docs/ROADMAP.md` 的 P0–P6 顺序推进。每阶段必须有来自原版行为的 fixture 或完整 scenario，
不能用源码行数、文件数量、局部 coverage、文档勾选或“已经能跑”宣称完成。

默认测试不得依赖 network、真实 credential 或 TypeScript runtime。Go 代码变更至少运行：

```sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
```

涉及 agent、streaming、session、tool 并发或取消时运行相关 race test。若基线已有失败，必须
确认并记录它是否与本次修改相关；不得用已知失败掩盖新增失败。

## 文档纪律

- `ARCHITECTURE.md` 记录长期边界，`ROADMAP.md` 记录依赖顺序，`STATUS.md` 只记录当前事实。
- 实现阶段或事实变化时直接更新对应文档，删除误导性叙事，不追加互相矛盾的历史说明。
- 上游 commit 更新只需在 `STATUS.md` 记录新的审查基线；只有行为差异值得长期保留。
- 重要行为由代码、测试和端到端 scenario 证明，文档不是完成证明。

## 工作区安全

- 不相关修改属于用户，保留并忽略，绝不使用 Git 回退命令。
- 永远不要删除文件；确实需要移出仓库时移动到 `/tmp` 并说明位置。
- 临时文件放在 `/tmp`，不要污染仓库。
- 搜索必须针对工作区或明确目录，不做全局 filesystem 搜索。
- 线性任务不委派；复杂多维任务需要委派时，明确禁止子 agent 继续派生。

确认无误后及时提交，不要让工作区变得越来越乱。
