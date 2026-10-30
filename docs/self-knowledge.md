# 版本、运行状态与本地源码

默认系统提示提供 README 和文档目录的绝对路径，以及按主题组织的文档导航。
程序版本可通过 `pi-go --version` 查询。文档和源码来自本次编译输入，随二进制内嵌，
首次建立运行时会安装到用户数据根下的
`knowledge/<构建标识>/`。这套资料可以直接用 `read`、`grep` 或 `bash` 阅读，不需要联网。

`manifest.json` 记录内置资料的版本、构建标识和原始文件 SHA-256。构建标识由版本与内置资料的
内容计算，同一版本号下的源码或文档变化也会产生不同目录。

已有的构建目录会直接复用；其中的文件可以修改、补充或移走，后续启动不会自动修复或覆盖这些改动。

普通 `go build`、开发入口和发布构建使用同一套 `go:embed` 输入，不依赖生成后可能过期的源码
副本。源码树包含 Go Core、同模块 Surface、文档、测试 fixture 与构建脚本；GUI 作为独立模块
额外提供自己的源码。依赖安装目录、凭据和生成的 UI 产物不进入资料树。

Agent 会读取本地修改后的文件内容。要改变程序执行逻辑，需要
重新编译；构建方式见 [发布说明](RELEASING.md)。

## 当前模型与会话

LLM 调用的 Bash 工具在每次执行时获得以下环境变量：

| 变量 | 内容 |
|---|---|
| `PI_EXECUTABLE` | 运行当前 Agent Core 的 pi-go 程序绝对路径 |
| `PI_SESSION_ID` | 当前会话 ID |
| `PI_SESSION_FILE` | 当前 JSONL 路径，内存会话不设置 |
| `PI_PROVIDER` | 当前选中的 Provider |
| `PI_MODEL` | 当前选中的模型 ID |
| `PI_REASONING_LEVEL` | 当前有效推理级别 |

```sh
printf 'model=%s/%s\nreasoning=%s\nsession=%s\nfile=%s\n' \
  "$PI_PROVIDER" "$PI_MODEL" "$PI_REASONING_LEVEL" "$PI_SESSION_ID" "$PI_SESSION_FILE"
```

程序路径从当前 pi-go 进程取得，会话信息从 AgentSession / Agent 的真实状态读取。切换模型、推理级别、会话或 reload 后，下一次
工具调用使用新值。变量描述的是 pi-go 选择的模型；代理服务内部如何路由应向该服务查询。
用户直接输入的 `!` / `!!` Bash 命令不注入这些会话变量，子进程继承的旧值也会先清除。

自定义 `SYSTEM.md` 按[配置](configuration.md)中的规则替换默认系统提示。
