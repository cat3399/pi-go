# 版本、运行状态与本地源码

默认系统提示给出本地资料根目录；`README.md` 和 `docs/` 都从该目录解析。如果指定了
`--docs-dir`，提示词分别给出内置 README 和替代文档目录的绝对路径。

正式版的源码和文档位于 `knowledge/<版本号>/`（例如 `knowledge/1.2.3/`）。终端、Web 和 GUI
共用同一份公共资料；如果先用终端版，再启动同版本的 GUI，只会补入 GUI 独有的源码。
已安装的文件可以修改、补充或移走，后续启动不会覆盖或补回。旧版的长哈希目录保留原状。

开发版使用 `knowledge/dev-<短指纹>/` 区分同一版本号下的不同编译输入。`manifest.json`
记录版本、完整构建指纹和原始文件 SHA-256；正式版的公共资料和附加模块分别记录。
这套资料可以直接用 `read`、`grep` 或 `bash` 阅读，不需要联网。程序版本可用 `pi-go --version` 查询。

普通 `go build`、开发入口和发布构建使用同一套 `go:embed` 输入，不依赖生成后可能过期的源码
副本。源码树包含 Go Core、同模块 Surface、文档、测试 fixture 与构建脚本；GUI 作为独立模块
额外提供自己的源码。依赖安装目录、凭据和生成的 UI 产物不进入资料树。

Agent 会读取本地修改后的文件内容。要改变程序执行逻辑，需要
重新编译；构建方式见 [发布说明](RELEASING.md)。

## 当前模型与会话

启用 `bash` 时，默认系统提示列出以下变量的用途。实际值在模型每次调用 `bash` 时提供：

| 变量 | 内容 |
|---|---|
| `PI_EXECUTABLE` | 运行当前 Agent Core 的 pi-go 程序绝对路径 |
| `PI_SESSION_ID` | 当前会话 ID |
| `PI_SESSION_FILE` | 当前 JSONL 路径，内存会话不设置 |
| `PI_SESSION_AGENT_DIR` | 当前用户数据目录（装配 CLI 会话访问时提供） |
| `PI_PROVIDER` | 当前选中的 Provider |
| `PI_MODEL` | 当前选中的模型 ID |
| `PI_REASONING_LEVEL` | 当前有效推理级别 |

```sh
printf 'model=%s/%s\nreasoning=%s\nsession=%s\nfile=%s\nagent_dir=%s\n' \
  "$PI_PROVIDER" "$PI_MODEL" "$PI_REASONING_LEVEL" "$PI_SESSION_ID" "$PI_SESSION_FILE" "$PI_SESSION_AGENT_DIR"
```

程序路径从当前 pi-go 进程取得，会话信息从 AgentSession / Agent 的真实状态读取。切换模型、推理级别、会话或 reload 后，下一次
工具调用使用新值。变量描述的是 pi-go 选择的模型；代理服务内部如何路由应向该服务查询。
用户直接输入的 `!` / `!!` Bash 命令不注入这些会话变量，子进程继承的旧值也会先清除。

Agent 通过 `terminal` 工具打开的持久终端在创建时注入稳定的会话变量（`PI_EXECUTABLE`、`PI_SESSION_ID`、`PI_SESSION_FILE` 及 CLI 会话访问变量），不注入会随会话变化的 `PI_PROVIDER`、`PI_MODEL` 和 `PI_REASONING_LEVEL`；查询当前模型状态使用 `bash`。所有持久终端（包括用户在面板中新建的终端）启动前都会清除进程继承的旧会话变量。

自定义 `SYSTEM.md` 按[配置](configuration.md)中的规则替换默认系统提示。
