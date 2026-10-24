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

自更新使用本地 HTTPS 发布服务器及真实 Go 可执行 fixture，覆盖不同 surface 的资产选择、
分页与版本排序、下载校验、terminal/web/gui 运行中程序替换、重装、降级、取消和失败恢复，
以及 Android APK 选择、下载链接和宿主计划过期。相关检查为
`go test -race ./internal/selfupdate ./internal/product ./surface/cli ./cmd/pi-go`。
Android 验收检查设置页读取实际安装版本、选中本平台最新 APK，以及通过系统浏览器打开下载。

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

## 数据读取性能验证

`internal/application/data_benchmark_test.go` 生成固定内容的 v3 JSONL 大规模会话，覆盖
1,000/5,000 个短会话、12,000 条消息的长会话、压缩后仅保留少量上下文的长会话，以及有旁支
和 custom message 的会话。正文、thinking、tool call/result 和 usage 走真实解码与投影路径。
生成与 Runtime admission 不计入查询耗时，生产路径不注入假数据。

```sh
DEEPSEEK_API_KEY= go test ./internal/application -run '^$' \
  -bench '^BenchmarkData' -benchtime=5x -count=5 -cpu=1 -benchmem
go test ./internal/surfacewire -run '^$' \
  -bench '^BenchmarkHistoryProjection$' -benchtime=1s -count=5 -cpu=1 -benchmem
```

记录 `ns/op`、`B/op`、`allocs/op` 和 `wire-B/op`。`CatalogWarm` 已预热目录摘要；
`HistoryUnopened` 每次通过未驻留的 SessionManager 读取，表示解析冷读，不声称 OS 页缓存
或物理磁盘处于冷态。`HistoryActive`、`ContextActive`、`StateActive` 与 `StatsActive`
测已打开 Runtime；`CatalogAfterAppend` 在计时外追加一条真实内存会话记录，再测目录读取。
`LiveActive` 单独测带版本的当前运行态读取，不编码 wire，也不复制已完成的历史；其成本不应
随旧消息数量增长。它与完整 `StateActive` 的字段和测量边界不同，不能直接当成同一响应的加速比。
`BenchmarkDataEventReplay` 独立测 Application 事件缓冲的最近 64/4,096 条事件回放和
保留窗口已满时的发布，不包含 SSE 编码和网络传输。
`B/op` 是每次查询的分配量，不是进程峰值内存；`wire-B/op` 包含临时目录路径，跨运行可能
有几个字节的长度差异，不用它替代响应内容的兼容性校验。
`BenchmarkHistoryProjection` 单独覆盖长正文、大 thinking、图片和工具参数，检查跳过正文解析
的优化是否增加了需要实际转换的富内容成本。

前后比较使用同一工具链、固定迭代数与并发设置，多次运行观察分布；较快场景可另用更长
`-benchtime` 校验，不能用单次噪声判断收益。保留优化前的测试二进制可以复跑基线，不需要
回退工作区。CPU/heap profile 与临时报告输出到 `/tmp`，不把一次机器的耗时写成产品承诺。

性能验收同时检查输出和状态语义：分页不遗漏条目，压缩不丢历史，分支/编辑保留旧路径，
重复回放不重复应用，统计保留完整日志口径，缓存失效与外部替换不返回陈旧数据。
只减少响应字节却仍重复解析全部历史的改动，应明确报告服务端成本。收益很小且复杂度
不相称的优化不实施，优先处理已测出的常用路径瓶颈。

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
