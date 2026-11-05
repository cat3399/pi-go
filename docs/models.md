# 模型配置

内置模型目录是默认值，**用户配置始终优先**。同步内置目录不会改写用户的 `models.json`、
`settings.json`、认证数据或动态 Provider 缓存。

Web、桌面 GUI 和 App 的「设置 → 模型」按内置、自定义渠道分组，支持查看和搜索模型、
保存 API Key，以及添加、编辑和移除自定义渠道。自定义渠道可手动添加模型，或从兼容服务
读取模型列表；编辑时保留已有的高级字段。

内置渠道可分别启用、禁用，也可「仅启用此内置渠道」；该操作只调整内置渠道，保留自定义
渠道与所有凭据。选择状态存入 `models.json` 的 `providers.<id>.enabled`，省略时默认启用。
这是 pi-go 的配置扩展：关闭后从可选模型和新会话的自动选择中排除，全部关闭不会回退成
全部启用。现有会话的当前模型、进行中的请求与恢复时保存的模型保持原行为；显式 CLI
模型参数仍按原版的认证和路由语义解析。配置保存会刷新活动 Runtime 的模型目录，
不替换 AgentState；`enabledModels` 仍只表示原版的模型循环范围。

`models.json` 位于用户数据目录，用于添加模型和覆盖 Provider / 模型属性。下面是一个本地
OpenAI Chat Completions 兼容服务的配置示例；地址和模型 ID 应替换成服务实际提供的值：

```json
{
  "providers": {
    "local": {
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:8080/v1",
      "apiKey": "${LOCAL_API_KEY}",
      "models": [
        {
          "id": "local-model",
          "name": "Local model",
          "reasoning": false,
          "input": ["text"],
          "cost": { "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0 },
          "contextWindow": 32768,
          "maxTokens": 4096
        }
      ]
    }
  }
}
```

然后选择 `local/local-model`，例如：

```sh
pi-go run --model local/local-model -p "hello"
```

`api` 必须使用当前生产装配支持的协议。模型列表的存在不保证某个 Provider、API 或认证路线
可用。Provider 的 `headers`、`compat`、`modelOverrides` 等配置按对应协议解析；完整契约见
`internal/model/runtime.go` 的 `ProviderConfig`、`Model` 与配置解析器。

默认选择放在 `settings.json` 的 `defaultProvider`、`defaultModel`；活动会话可以通过
`/model` 切换。推理级别通过 `/thinking` 调整，实际有效级别受模型能力约束。`/reload`
重新加载目录配置；当前会话状态由核心管理。

运行中查询实际模型与推理级别的方法见 [运行状态](self-knowledge.md)，认证方式见
[Provider](providers.md)。

## 同步内置模型

开发时在仓库根目录运行：

```sh
make sync-models
make sync-models ARGS='-source /path/to/models.dev.json'
```

默认直接读取 [models.dev 的公开 JSON 接口](https://models.dev/api.json)，不下载 pi 的 npm
包，也不解析 JavaScript。`-source` 可以回放保存的同一接口响应，以重现或回退模型资料；
原来的 `-version` 参数已移除。完成后终端会显示新增、移除、参数变更等差异。

`internal/model/catalogdata/policy.json` 是随 Go 程序发布的本地适配规则，维护有序的默认模型、
已实现的 Provider 身份、协议、地址、特殊模型列表和兼容性修正。同步不会改写该文件。
已有模型的显式兼容参数、推理等级映射及上下文策略保留在这里；新增模型依据 models.dev 的
能力信息和已有 Go 协议适配规则生成。新增协议或修改本地适配规则仍需发布新版程序。

生成结果保存在 `internal/model/catalogdata/catalog.json`，记录数据源地址、源响应与本地规则的
SHA-256。每个在线模型的 `modelsDev` 字段保留原始模型对象，包括尚未投影的字段。价格按每百万
token 映射，并保留可表示的长上下文价格档位；本地兼容规则优先于通用模型资料。

只有支持工具调用的聊天模型进入目录；分类和图像生成模型不会进入 Agent 的聊天模型列表。
`azure` 映射到已注册的 `azure-openai-responses`，仅接受该适配器支持的 OpenAI 模型，不启用
Foundry 的其他协议。`togetherai` 映射到 `together`，并排除已标记退役的模型。现有认证、配置和
会话引用无需改名。models.dev 没有独立的 Codex OAuth 目录，因此 `openai-codex` 使用本地规则中
明确列出的模型，不从普通 OpenAI API 的模型列表推断订阅可用性。

已安装的程序也可以独立更新数据，无需重新编译：

```sh
pi-go models update
pi-go models update --source /path/to/models.dev.json
pi-go models update --agent-dir /path/to/agent-data
```

该命令将完整目录原子写入用户数据目录的 `builtin-models.json`。下次启动或 `/reload` 时，
Runtime 使用这份目录作为内置基线；文件不存在时使用二进制嵌入的数据。数据无效时保留进程内
上一份有效基线，首次启动则使用嵌入数据，并给出诊断。不会在启动时联网。
旧版已安装目录可直接读取，下次更新时写入新格式，不再访问旧数据源。

合并顺序仍为：内置基线 → 已注册 Provider 的动态目录缓存 → 用户 Provider / 模型定义与
`modelOverrides`。默认模型选择仍先考虑显式选择、会话、scope 和用户 `settings.json`，
只有需要自动回退时才使用内置 Provider 默认模型。已安装目录整体替换嵌入基线，所以上游移除的
模型不会被旧的嵌入数据重新补回；用户自己的模型定义仍然保留。

同步器与 Runtime 共用数据格式及解析逻辑，完整下载、解析后才发布单个文件，失败不替换旧目录。
支持的 Provider 身份、认证逻辑和协议实现继续由 Go 代码负责；新增协议能力仍需实现相应适配器。
固定的上游模型示例保存在测试 fixture 中，与日常更新的数据分开维护。
