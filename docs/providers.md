# Provider 与认证

在 TUI 中用 `/login` 选择 Provider，或在 Web / GUI 的模型配置界面管理认证。
生产 Provider 装配的支持范围由 `internal/app/production_models.go` 及
`internal/model/builtin_provider_lookup.go` 定义。

可以通过 Provider 对应的环境变量提供凭据，例如 `OPENAI_API_KEY`、`ANTHROPIC_API_KEY` 和
`DEEPSEEK_API_KEY`。持久化凭据存储在用户数据目录的 `auth.json`：

```json
{
  "openai": {
    "type": "api_key",
    "key": "${OPENAI_API_KEY}"
  }
}
```

凭据值支持字面量、`$NAME` / `${NAME}` 环境变量模板，以及以 `!` 开头的取值命令。
`$$` 表示字面量 `$`。Provider 的 `models.json` 也可以提供 `apiKey` 和请求头模板。
配置文件中的未知字段在对应的持久化修改路径中保留。

OAuth 凭据由登录流程创建，并由认证运行时在需要时刷新。不要手工猜测或生成 OAuth 字段。
本地 OAuth 服务包括 OpenAI Codex 和 Anthropic；它们的协议实现位于 `internal/auth`。
Windows 暂不支持凭据文件的安全读写；没有 `auth.json` 时可以使用环境变量或运行时凭据，
已有凭据文件会返回读取权限检查不受支持的错误。

没有模型时先完成认证，再用 `/model` 选择模型。配置自定义服务见 [模型配置](models.md)。
用户数据目录与导入规则见 [配置](configuration.md)。
