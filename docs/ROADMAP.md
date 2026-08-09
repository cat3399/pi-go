# 后续开发计划

本计划只描述下一阶段的大方向。实现顺序以原版架构和行为依赖为准，不以尽快接上 pi-web
为目标，也不允许让 transport 或前端补偿 Go 核心缺失的语义。

## 开发原则

- 以原版实际生产源码和测试为基准，Go 代码形式可以不同，行为不能降级。
- 一个概念只保留一个状态 owner，不引入第二套 Agent/session 状态。
- fake 只用于测试，未实现能力必须明确暴露。
- TUI、主题和 JS extension UI 可以后置，核心 hook、resource 和 custom data 契约不能省略。

## 第一阶段：关闭 Agent 核心差异

先修正当前已经确认的架构和生命周期差异：

- 收敛 AgentSession 与 Agent 的状态所有权；
- 对齐 settled、queue、hook、message 和 branch-summary 等事件语义；
- 核对 retry、overflow、abort 和 queued continuation 的组合行为。

参考：

- `../pi/packages/agent/src/agent.ts`
- `../pi/packages/agent/src/agent-loop.ts`
- `../pi/packages/agent/src/types.ts`
- `../pi/packages/coding-agent/src/core/agent-session.ts`

## 第二阶段：完成产品级 AgentSession

把原版由 AgentSession 统一编排的能力收敛到同一条 Go 产品路径：

- prompt/templates/skills/commands；
- tool registry、active tools、system prompt 和 reload；
- standalone bash 的 Host/settings 集成、model/thinking 和 session 产品操作。

参考：

- `../pi/packages/coding-agent/src/core/agent-session.ts`
- `../pi/packages/coding-agent/src/core/sdk.ts`
- `../pi/packages/coding-agent/src/core/system-prompt.ts`
- `../pi/packages/coding-agent/src/core/agent-session-services.ts`
- `../pi/packages/coding-agent/src/core/bash-executor.ts`

## 第三阶段：补齐 Provider、Model、Auth 和工具

先让现有 adapter 和服务忠实实现通用契约；Provider 数量扩展不作为内部核心验收的前置条件：

- 完整 model/provider 配置、stream options、usage/cost 和错误语义；
- catalog、models.json、auth/login 和专用 Provider 路径；
- 内置工具与原版行为对齐。

参考：

- `../pi/packages/ai/src/types.ts`
- `../pi/packages/ai/src/providers/`
- `../pi/packages/ai/src/api/`
- `../pi/packages/coding-agent/src/core/model-runtime.ts`
- `../pi/packages/coding-agent/src/core/provider-composer.ts`
- `../pi/packages/coding-agent/src/core/tools/`

## 第四阶段：完成 Runtime 边界和整体验收

建立 transport-neutral 的长期 Runtime/Host：

- command result、权威 state snapshot 和单一有序 event stream；
- canonical wire DTO、session identity、replacement 和 shutdown；
- TypeScript/Go 共用场景，覆盖完整 AgentSession/Runtime workflow。

参考：

- `../pi/packages/coding-agent/src/core/agent-session-runtime.ts`
- `../pi/packages/coding-agent/src/modes/rpc/rpc-types.ts`
- `../pi/packages/coding-agent/src/modes/rpc/rpc-mode.ts`
- `../pi/packages/protocol/src/schemas.ts`
- `../pi/packages/server/src/types.ts`

只有这一阶段通过后，内部 Agent 重写才算完成。

## 第五阶段：统一 Surface 架构与原生 WebUI（进行中）

Next → `pi-go-rpc` 接入已经完成可行性验证，不再作为产品兼容层继续开发。长期目标是在 pi-go
仓库内提供可选编译的 `pi-go-web`，直接进程内装配 Host/Runtime，并为 TUI、WebUI 和未来 GUI
建立同一套 Application contract。

已完成：

1. 固化共享 Host JSON command/result/event projection 与多 Session supervisor，不建立 UI 影子状态；
2. 以现有 pi-web 为基准迁移布局、主题、响应式样式和静态资源，并通过静态构建；
3. 接通 Agent chat、SSE、session browsing/restore/context/tree/state/rename 与基础 model discovery/selection；
4. 使用 DeepSeek V4 Flash 完成真实 `read` 工具短程验收。

后续按高内聚模块推进：

1. files/file-index/preview/watch/upload 与 Git status/diff；
2. worktree 和目录选择；
3. models-config/auth/provider 管理；
4. session export/auto-name/delete/thinking block 延迟读取；
5. 前端 capability gating，再逐步处理明确暂缓的插件、extension custom UI 和 skills 管理。

持续约束：Go HTTP/SSE 必须直接调用 Host，浏览器事件投影不改变 canonical Agent 语义；静态
前端只嵌入可选 Web 二进制，默认核心构建不包含 Web 资源。

视觉完成和能力完成分开记录。未实现能力必须在 capability manifest 和 `docs/WEBUI.md` 中可见，
API 返回结构化 unsupported；前端禁用状态是当前明确待办，禁止用占位响应、假 session、假模型
或静态效果制造完成错觉。

参考：

- `../pi-web/app/globals.css`
- `../pi-web/components/`
- `../pi-web/hooks/`
- `../pi-web/app/api/`
- `internal/host/`
- `internal/runtime/`

## 验证节奏

- 每个小改动使用本地单元测试、HTTP fixture、浏览器构建检查和短程任务；
- 一个高内聚大模块完整闭环后，再使用 DeepSeek 做真实只读验收；
- 大模块验收同时检查 UI、HTTP/SSE、Host event、session JSONL 和 shutdown，不把模型返回一段
  文本当作整体完成证明。
