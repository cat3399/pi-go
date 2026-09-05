# 模型配置

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
