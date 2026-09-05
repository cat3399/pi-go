# 工具、技能与提示模板

默认活动工具为 `read`、`bash`、`edit`、`write`。注册表同时提供 `grep`、`find`、`ls`，
活动工具由当前会话控制。TUI 用 `/tools` 调整；系统提示根据实际活动工具重建。

`read` 支持文本和图片，文本可用 `offset` / `limit` 分段读取；`write` 写入完整文件；
`edit` 对匹配的文本进行替换；`bash` 执行工作目录中的命令，并报告截断信息和完整输出文件。
准确参数以当前工具 schema 为准，实现见[工具定义](../internal/tool/registry.go)。

查阅产品文档时，从系统提示提供的 README 和文档目录开始。工具的相对路径以会话工作目录
为基准；文档中的相对链接以该文档所在目录为基准，传给工具时使用解析后的绝对路径。
当前模型和会话信息见[运行状态](self-knowledge.md)。

## 技能

用户技能位于用户数据目录的 `skills/`；项目技能位于 `.pi-go/skills/`。共享
`~/.agents/skills/` 和项目祖先目录中的 `.agents/skills/` 也参与发现。项目资源遵循信任设置。

每个技能目录包含 `SKILL.md`：

```markdown
---
name: review
description: Review the project's code and explain actionable findings.
---
Read the relevant code and tests before producing the review.
```

系统提示列出可用技能的名称、说明和文件位置，Agent 按需读取正文。也可使用
`/skill:review <任务>` 显式调用。`disable-model-invocation: true` 将技能从自动发现提示中隐藏，
显式调用仍可使用。技能内的相对引用以技能目录为基准。

## 提示模板

用户模板位于用户数据目录的 `prompts/`；项目模板位于 `.pi-go/prompts/`。例如
`prompts/review.md`：

```markdown
---
description: Review a specific component
---
Review $1, with attention to $ARGUMENTS.
```

使用 `/review parser` 展开模板。模板支持位置参数、默认值和参数切片，完整语法实现见
[模板展开实现](../internal/resource/prompt.go)。资源配置、信任和 `/reload` 见 [配置](configuration.md)。

## 扩展契约

Go 核心提供工具结果、事件、自定义消息、生命周期 hook 和资源来源等 typed 契约，供进程内
装配使用。TypeScript 扩展 / npm 包的发现和执行不在当前生产加载器的能力范围内。
接口定义见[扩展契约](../internal/agent/extensions.go)，状态所有权见 [核心架构](ARCHITECTURE.md)。
