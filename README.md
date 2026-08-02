# pi-go

pi-go 的目标是用 Go 完整重写 [pi](https://github.com/cat3399/pi)，保留其核心产品
行为，并在核心稳定后让 pi-web 通过少量启动与传输层修改接入 Go runtime。

产品运行时只使用 Go，不嵌入、代理或 fallback 到 TypeScript 版 pi。原版 pi 的实际
实现和测试是行为参考；文档只记录当前共识，不能代替源码成为设计依据。

## 当前阶段

仓库已经具备一批经过测试的底层能力，包括 OpenAI Responses streaming、tool loop、
内置工具、JSONL session、auth/model/resource 加载，以及 agent 的队列、重试和压缩
相关实现。

但当前还不是完整的 Pi，也不适合直接作为 pi-web backend：

- 已有产品级 `AgentSession` 和每轮不可变 snapshot；模型、thinking、system prompt 和
  tools 可以在运行期间改变，并作用于 tool chain 的下一次 provider 请求；
- 通用 model/provider/message 边界仍受 OpenAI Responses 数据形状影响；
- 图片和富工具结果只在部分底层类型中存在，没有贯通产品运行路径；
- 重试、自动压缩、队列和 TUI 等已实现能力尚未全部形成完整长期 runtime；
- CLI 目前只是单次 `-p` headless 运行，没有长期运行的 RPC/session runtime。

详细事实见 [当前状态](docs/STATUS.md)，目标边界见
[核心架构](docs/ARCHITECTURE.md)。

## 当前可运行入口

现有 executable 只支持显式的 print prompt：

```sh
OPENAI_API_KEY=... go run ./cmd/pi-go -p "检查当前目录"
```

也可以显式选择 model 或 session 文件：

```sh
go run ./cmd/pi-go --model gpt-5.5 --session /absolute/path/session.jsonl -p "继续处理"
```

当前 production assembly 只支持 OpenAI Responses。配置、credential 或模型不可用时会
明确失败，不会切换到其他 runtime。

## 开发检查

```sh
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go build ./...
```

涉及 agent、streaming、session、tool 并发或取消时，还应运行相关 race test。测试应
默认使用 deterministic fake，不依赖真实 credential 或网络。

## 文档

- [核心架构](docs/ARCHITECTURE.md)：下一阶段必须形成的长期边界。
- [当前状态](docs/STATUS.md)：已经实现、已经接入和仍然缺失的能力。
- [近期路线](docs/ROADMAP.md)：核心重构与集成顺序。

不再为每个小模块维护 charter、behavior ledger、test ledger 或 review 流水账。重要
行为由代码和测试证明；影响整体方向的差异才写入上述文档。
