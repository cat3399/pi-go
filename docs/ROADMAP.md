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

## 第五阶段：接入 pi-web（已启动）

长期 JSONL Runtime 和 pi-web 薄进程 adapter 已落地并可 opt-in 使用。下一步按 pi-web 的真实
需求补充辅助 command、恢复/切换场景和 Provider/model/auth breadth，逐步替换服务端直接使用的
TypeScript Agent 核心。pi-web 继续负责 HTTP/SSE、连接恢复和 UI 投影，不保留第二套 Agent
状态或产品策略。

参考：

- `../pi-web/lib/pi-types.ts`
- `../pi-web/lib/rpc-manager.ts`
- `../pi-web/lib/session-reader.ts`
- `../pi-web/hooks/useAgentSession.ts`
- `../pi-web/app/api/models/`
- `../pi-web/app/api/auth/`

完成目标是让主要改动集中在 pi-web 的服务端启动和 transport adapter，React 交互逻辑无需
为 Go 后端重写。
