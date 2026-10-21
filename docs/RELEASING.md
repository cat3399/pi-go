# 构建与发布

## 本地构建

所有命令从仓库根目录执行，`SURFACE` 可选 `terminal`（默认）、`web`、`gui` 或 `mobile`：

```sh
make help
make setup SURFACE=web
make check SURFACE=web
make build SURFACE=web
make dev SURFACE=web ARGS='--cwd /path/to/project'
make run SURFACE=web ARGS='--cwd /path/to/project'
```

Terminal、Web 和 GUI 的 `make run` 使用已有产物；Mobile 的设备运行会先构建再安装。
Terminal 只需 Go 工具链，包含 CLI、TUI、RPC 和 Web API。其他平台的工具链与开发步骤见
[Web](../surface/web/_frontend/README.md)、[GUI](gui.md)和[Android](mobile.md)。

通过 `VERSION` 指定版本，`OUTPUT_DIR` 指定构建产物目录：

```sh
make build SURFACE=terminal VERSION=1.2.3
make build SURFACE=web VERSION=1.2.3 OUTPUT_DIR=/tmp/pi-go-build
```

Terminal 和 Web 可以用 Go 环境变量交叉编译，例如 Linux AMD64：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make build SURFACE=web
```

## GitHub Actions

在 GitHub 仓库的 **Actions** 页面中：

1. 日常取包选择 **Build**，再选择需要的 surface；
2. 正式发布选择 **Release**。默认的 `all + patch` 会创建下一个补丁版本并构建全部 surface；
3. 只在不兼容变更或新增能力时把升级级别改为 `major` 或 `minor`。

不需要手工修改源码版本、创建 tag、整理压缩包或计算校验和。Release 必须从默认分支运行，多个
Release workflow 会串行执行，避免并发任务同时修改同一个版本。

## 版本规则

- 只把精确匹配 `vMAJOR.MINOR.PATCH` 的 tag 视为稳定版本；
- 仓库没有稳定 tag 时，首个自动发布版本为 `v0.1.0`；
- 所有 surface 共用同一个版本。选择 `all` 时不检查最高稳定版本的资产是否齐全，始终按照 `patch`、
  `minor` 或 `major` 创建新版本并重新构建全部 surface；
- 单独选择 surface 时，若它尚未完整出现在最高稳定版本中，Release 会把它追加到现有 tag 和
  GitHub Release，不递增版本；已经完整存在时才按照所选级别创建新版本；
- **Build** 不占用版本号。选择 `all` 时使用下一补丁版本作为 SemVer 基准；单独选择 surface 时使用
  同一套缺失判断，最后统一追加 `-dev.<commit>`；
- 同一版本会注入 Go CLI、内嵌 Web UI、GUI bridge、移动端 Go 库，以及 Android 的
  `versionName`。Android `versionCode` 由 SemVer 单调映射生成。

## 产物

| Surface | Release 产物 |
|---|---|
| `terminal` | Linux/macOS/Windows 的 amd64 与 arm64 压缩包 |
| `web` | 内嵌 Web UI 的 Linux/macOS/Windows amd64 与 arm64 压缩包 |
| `gui` | Linux amd64、Windows amd64、macOS amd64 与 arm64 原生包 |
| `mobile` | Android arm64 APK |

所有发布资产旁会生成 `SHA256SUMS`。向已有版本补充 surface 时，workflow 会保留原资产的校验和、
合并新资产校验和，并更新发布说明中的完整 surface 列表。

Terminal/Web/GUI 的打包脚本注入 `internal/product.Distribution=archive`；普通源码构建默认 `source`。
Terminal/Web 的身份来自实际编译标签 `pi_go_webui`，GUI/mobile 由各自独立宿主绑定，
不从文件名、当前连接的 Core 或启动子命令推断。这些信息用于[程序自更新](usage.md#更新程序)。
打包时需保持发布文件命名和压缩包根目录的单个程序结构（CLI 为 `pi-go[.exe]`，GUI 为
`pi-go-gui[.exe]`）；Android 保持应用 ID、版本编码与签名身份。补充 surface 时，只有资产及合并后的校验记录都发布完成，
该版本才会成为这一 surface 的可更新版本。

包含 Core 的二进制同时内嵌本次编译的源码和使用文档，普通 `go build` 也具备该能力；GUI
额外内嵌独立 GUI 模块的源码输入。`internal/product.Version` 是 Core 与 Surface 共用的版本
注入点。资料安装位置、本地自定义和版本对应关系见[本地源码](self-knowledge.md)。

## 缓存

构建按照工具链分 job，缓存彼此隔离：

- Go module 和编译缓存按 runner、架构及对应 `go.sum` 区分；
- npm 下载缓存按各 frontend 的 lockfile 区分；
- Web 额外复用 Next 增量构建缓存，六个目标只执行一次前端生产构建；
- Android 缓存 Gradle 依赖与 wrapper、固定版本的 NDK 和 Wails CLI；Task 使用固定版本的预编译工具，
  避免为运行构建任务额外编译 Task 自身。

所有引用的第三方 GitHub Actions 都固定到完整 commit SHA，避免浮动 tag 在无代码变更时改变构建。

## 签名边界

Android **Build** 生成 debug APK，**Release** 生成 release APK；CI 两者使用同一个固定证书，
可以相互覆盖升级。该证书源自 Android debug keystore，release 构建类型不会自动改变签名身份。
预期的 SHA-256 指纹定义在 `_build-surfaces.yml` 的 `EXPECTED_ANDROID_CERT_SHA256`，
workflow 在构建前后校验。更换签名证书会影响已有安装的覆盖升级。

GitHub Actions 使用 `ANDROID_KEYSTORE_BASE64`、`ANDROID_KEYSTORE_PASSWORD`、`ANDROID_KEY_ALIAS` 和
`ANDROID_KEY_PASSWORD` 四个仓库 Secret。keystore 本身不得提交到仓库。本机普通构建继续使用 Android
默认的 `~/.android/debug.keystore`；本地产物仅在证书与 CI 相同时可以互相覆盖升级。
需要检查 release 构建时可运行：

```sh
PI_GO_ANDROID_BUILD_TYPE=release make build SURFACE=mobile VERSION=1.2.3
```

GUI 产物尚未进行 Windows 代码签名或 Apple Developer ID 签名、公证。

如果 Release job 在创建 tag 时收到权限错误，请确认仓库 **Settings → Actions → General → Workflow
permissions** 允许 `GITHUB_TOKEN` 写入仓库内容。workflow 本身只在发布 job 请求 `contents: write`，
无需个人访问令牌。
