# 配置与目录

| 内容 | 默认位置 |
|---|---|
| 用户设置 | `~/.pi-go/agent/settings.json` |
| Provider 凭据 | `~/.pi-go/agent/auth.json` |
| 模型配置 | `~/.pi-go/agent/models.json` |
| 会话 | `~/.pi-go/agent/sessions/<编码后的工作目录>/` |
| 项目目录列表 | `~/.pi-go/agent/projects.json` |
| 项目信任记录 | `~/.pi-go/agent/trust.json` |
| 用户技能、模板 | `~/.pi-go/agent/skills/`、`prompts/` |
| 项目设置、技能、模板 | `<项目>/.pi-go/settings.json`、`skills/`、`prompts/` |
| 内置源码和文档 | `~/.pi-go/knowledge/<构建标识>/` |

`PI_GO_AGENT_DIR` 覆盖用户数据目录；支持 `--agent-dir` 的入口以该参数为最高优先级。相对路径
相对于指定的工作目录解析，`~` 展开为用户主目录。内置资料放在所选 agent 目录的同级
`knowledge` 目录下。`--help` 与 `--version` 不初始化数据目录。

TUI、Web 和 GUI 的 `--docs-dir` 可以指定替代文档目录。该选项只覆盖文档位置，
README 和源码位置由随二进制携带的资料决定。

全局 `settings.json` 示例：

```json
{
  "defaultProvider": "openai",
  "defaultModel": "gpt-4.1",
  "defaultThinkingLevel": "medium",
  "steeringMode": "one-at-a-time",
  "followUpMode": "one-at-a-time",
  "compaction": { "enabled": true },
  "retry": { "enabled": true },
  "images": { "autoResize": true }
}
```

项目设置在项目信任后参与合并。用户和项目的 `SYSTEM.md` 可以替换默认系统提示；
`APPEND_SYSTEM.md` 追加指令，同类文件优先选择项目版本。项目根目录和祖先目录的
`AGENTS.md` / `CLAUDE.md` 仍作为共享项目指令加载。共享 `.agents/skills` 的发现规则保持独立。
修改后可使用 `/reload`。

## 首次导入

首次使用默认用户目录且目标不存在时，pi-go 从 `~/.pi/agent` 复制兼容数据。若设置了
`PI_CODING_AGENT_DIR`，它只用于选择这次导入的来源。显式设置 `PI_GO_AGENT_DIR` 或
`--agent-dir` 表示使用独立的指定目录，不自动从其他用户目录导入。

导入包括配置、凭据、会话、技能、模板、用户指令，以及已有的 pi-go 项目和信任记录。项目的
`.pi` 在首次使用该项目时单独导入 `.pi-go`。已存在的目标目录是权威数据，不做合并或覆盖，
后续启动不再从旧目录加载默认资源。

导入保留来源，通过临时目录完成复制后一次性发布目标。完成记录在目标的 `.migration.json`，
包括复制文件、跳过的资源和格式诊断。中途失败保留临时目录，目标保持不存在，下次可以重试。
并发启动由进程锁串行化，进程退出后锁自动释放。

设置中的技能、模板和会话目录引用，以及会话头部的 `parentSession`，会按导入范围重新定位。
树内符号链接指向新位置；指向外部共享资源的链接保留其外部引用。凭据和配置文件会成为新的
私有普通文件。自定义指令、命令内容和历史消息保持原内容，不对任意文本做路径替换。

TypeScript 扩展、扩展安装目录、主题运行资源、工具下载缓存和进程锁不属于自动导入的运行能力。
原始文件继续保留，跳过项记录在导入清单。自定义 `SYSTEM.md` 仍按用户指定的替换语义生效。

完整设置字段以 `internal/model/runtime.go` 的 `Settings`、校验和默认值方法为准；
认证与模型配置分别见 [Provider](providers.md) 和 [模型](models.md)。
