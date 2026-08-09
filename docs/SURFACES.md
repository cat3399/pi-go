# Surface 架构

## 目标

pi-go 只有一套 Agent/Application Core。TUI、WebUI、未来 GUI 和 RPC 自动化都是可选 surface
或 transport，不复制 Runtime、AgentSession、Agent、SessionManager 的产品状态和策略。

```text
TUI / WebUI / GUI / RPC
            │
            ▼
Application Host: typed commands, queries, events, capabilities
            │
            ▼
Session Supervisor: independent Host/Runtime lifecycles
            │
            ▼
Runtime → AgentSession → Agent → AgentLoop
```

## 共享与不共享

共享：

- prompt、queue、abort、retry、compaction、model、thinking、tools、session 等产品命令；
- 权威 state/query；
- canonical AgentSession event；
- session discovery、identity 和生命周期规则；
- approval、notification、clipboard、file picker 等可选 capability contract。

不共享：

- 终端 raw mode、快捷键和主题渲染；
- 浏览器路由、面板、滚动、SSE 重连和 DOM 更新；
- GUI 窗口、菜单、系统通知和 native dialog；
- 为求统一而抽象出的通用按钮、通用窗口或其他最低公分母 UI 框架。

## 状态所有权

- durable conversation/session state 只在 SessionManager/store；
- active model/tools/messages/queue/run state 只在 Agent/AgentSession；
- Runtime/Host 只负责装配、replacement、命令和有序事件；
- Supervisor 只持有独立 Runtime/Host 句柄及生命周期元数据；
- surface 只保留选中标签、面板尺寸、输入草稿等呈现状态。

Web/GUI 重连时读取权威 snapshot 和 durable session，而不是回放事件构造第二套 Agent 状态。

## 构建边界

各 surface 使用独立 composition root：

- `cmd/pi-go`：现有默认入口；
- `cmd/pi-go-rpc`：外部自动化与协议验收；
- `cmd/pi-go-web`：可选 WebUI；
- 未来 `cmd/pi-go-gui`：可选 GUI。

默认构建不导入 Web/GUI 包。静态资源与 surface 专用依赖只进入对应二进制；build tag 只选择
生成资源和对应 composition root，不切分 Host/Runtime 产品逻辑，避免形成两套难以完整测试的行为。

## WebUI 特有边界

Go Web Host 直接调用 Application Host。HTTP 负责请求验证和命令映射，SSE 负责有序事件投影、
连接生命周期与背压。当前重连通过权威 state 与 durable session 恢复；显式 sequence/cursor replay
仍是后续可靠性工作。Web 投影可以省略浏览器不使用的字段或合并视觉更新，
但 Go 内部 canonical event 和 durable session 必须保持完整。

浏览器界面可以继续使用 TypeScript/React；生产运行时服务端仍只有 Go。前端构建产物作为
真实静态资源进入可选 Web 二进制，不允许用静态假消息或演示数据替代 API。

当前 `internal/webui.Supervisor` 为每个活动会话持有一个独立 Host/Runtime，完成并发打开去重、
空闲回收和 shutdown；`internal/hostjson` 是 JSONL RPC 与 Web HTTP/SSE 共用的命令、结果和事件
投影。Web 请求不启动 RPC 子进程，也不通过 Next Server 转发。
