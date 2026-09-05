# 模型配置

内置模型目录是默认值，**用户配置始终优先**。同步内置目录不会改写用户的 `models.json`、
`settings.json`、认证数据或动态 Provider 缓存。

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
make sync-models ARGS='-version <上游发行版本>'
```

入口位于 `scripts/sync-models.sh`，Go 驱动位于 `scripts/sync-models/`。默认同步最新正式发行包，也可以指定版本来重现或回退数据。
完成后终端会显示新增、移除、参数变更、默认模型变更及 Provider 元数据变更；不生成报告文件。

仓库数据统一保存在 `internal/model/catalogdata/catalog.json`，包含模型原始 JSON、有序的
Provider 默认模型、默认 API / 地址和上游包版本、commit、完整性信息。同步读取同一发行版本的
`pi-ai` 模型数据和 `pi-coding-agent` 默认选择，不执行 TypeScript，也不生成模型 ID、版本号或
文件校验值的 Go 常量。原始模型的未知字段会保留在数据中。

已安装的程序也可以独立更新数据，无需重新编译：

```sh
pi-go models update
pi-go models update --version <上游发行版本>
pi-go models update --agent-dir /path/to/agent-data
```

该命令将完整目录原子写入用户数据目录的 `builtin-models.json`。下次启动或 `/reload` 时，
Runtime 使用这份目录作为内置基线；文件不存在时使用二进制嵌入的数据。数据无效时保留进程内
上一份有效基线，首次启动则使用嵌入数据，并给出诊断。不会在启动时联网。

合并顺序仍为：内置基线 → 已注册 Provider 的动态目录缓存 → 用户 Provider / 模型定义与
`modelOverrides`。默认模型选择仍先考虑显式选择、会话、scope 和用户 `settings.json`，
只有需要自动回退时才使用内置 Provider 默认模型。已安装目录整体替换嵌入基线，所以上游移除的
模型不会被旧的嵌入数据重新补回；用户自己的模型定义仍然保留。

同步器与 Runtime 共用数据格式及解析逻辑，完整下载、解析后才发布单个文件，失败不替换旧目录。
支持的 Provider 身份、认证逻辑和协议实现继续由 Go 代码负责；新增协议能力仍需实现相应适配器。
固定的上游模型示例保存在测试 fixture 中，与日常更新的数据分开维护。
