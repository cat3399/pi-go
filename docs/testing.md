# 测试与兼容性验证

Go Core 的兼容性证据记录在 [capability 清单](parity/core-a116523.yaml)，固定上游 commit 为
`a116523434806910336b9de3e38a41aa5860030b`。清单区分冻结 scenario 验证、依据源码契约实现并测试，
以及仍存在的集成差异；它记录 Core 范围，不是当前各 Surface 的功能列表。

## 本地检查

```sh
make check SURFACE=terminal
make e2e-core
```

`e2e-core` 覆盖 production assembly、Application、RPC 与 Web。普通测试使用 deterministic
provider 或本机测试服务器，验证请求、工具循环、事件、JSONL 持久化与恢复。涉及并发与生命周期
的修改同时执行 `go test -race`，并运行相关包的 `go vet` 和构建。

图形与移动宿主分别检查：

```sh
make check SURFACE=web
make check SURFACE=gui
make check SURFACE=mobile
```

## 上游 fixture

冻结 oracle 由上游实际生产入口生成，Go 测试直接读取已提交的结果。运行测试不需要启动
TypeScript pi。生成方式、环境版本与归一化范围见：

- [AgentSession workflow](../internal/agent/testdata/README.md)
- [资源解析与模板](../internal/resource/testdata/README.md)
- [工具与文件发现](../internal/tool/testdata/README.md)

更新 oracle 时核对上游 revision 与完整结果差异。pi-go 的产品名称、独立目录和安装资料是明确的
产品差异；兼容性测试保留上游原始 fixture，并只在相应比较边界归一化目录名称。

## 真实 Provider 验收

DeepSeek 网络测试由 `DEEPSEEK_API_KEY` 显式启用，会产生模型调用费用：

```sh
DEEPSEEK_API_KEY='...' make e2e-deepseek
```

该测试覆盖真实 transport、工具循环、压缩与分支摘要，并构建实际二进制，跨进程验证默认工具、
JSONL 持久化和恢复。运行普通本地验证时可显式清空该变量：

```sh
DEEPSEEK_API_KEY= go test ./...
```
