# 首批 module charter

这些 charter 是阶段 0 对首个 standalone workflow 的设计假设，不是 public API 或
最终 package layout。每个文件声明职责、证据、ownership、首批 behavior 和独立
review gate；随着新行为证据调整时，必须同步更新 ledger 与受影响 workflow。

- [BASE_SEMANTICS.md](BASE_SEMANTICS.md) — M-BASE 基础 message/content/stream 语义；
- [AI_PROVIDER.md](AI_PROVIDER.md) — M-PROVIDER deterministic fake 与 provider runtime；
- [AGENT_RUNTIME.md](AGENT_RUNTIME.md) — M-AGENT 单 active run 与 tool loop；
- [SESSION_STORAGE.md](SESSION_STORAGE.md) — M-SESSION v3 JSONL 与 durable invariant；
- [TOOL_SYSTEM.md](TOOL_SYSTEM.md) — M-TOOL Bash 与 filesystem tool suite；
- [APPLICATION_CLI.md](APPLICATION_CLI.md) — M-APP text print 与 WF-001 装配。
- [AUTH_STORAGE.md](AUTH_STORAGE.md) — M-AUTH API-key storage、runtime overlay 与 config resolution。

阶段 0 之后的产品服务、Interactive/TUI、Extension 和 Remote 模块仍以
[../../ARCHITECTURE.md](../../ARCHITECTURE.md) 与 [../../ROADMAP.md](../../ROADMAP.md)
为范围来源，在其首个 slice 前再建立对应 charter，不能提前创建空 package。
