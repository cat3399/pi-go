# 构建与发布

## 最少操作

在 GitHub 仓库的 **Actions** 页面中：

1. 日常取包选择 **Build**，再选择需要的 surface；
2. 正式发布选择 **Release**。默认的 `all` 和 `patch` 会构建全部 surface，并自动发布下一个补丁版本；
3. 只在不兼容变更或新增能力时把升级级别改为 `major` 或 `minor`。

不需要手工修改源码版本、创建 tag、整理压缩包或计算校验和。Release 必须从默认分支运行，多个
Release workflow 会串行执行，避免并发任务使用同一个版本号。

## 版本规则

- 只把精确匹配 `vMAJOR.MINOR.PATCH` 的 tag 视为稳定版本；
- 仓库没有稳定 tag 时，首个自动发布版本为 `v0.1.0`；
- 后续版本以最高稳定 tag 为基准，按照选择的 `patch`、`minor` 或 `major` 自动递增；
- **Build** 不占用版本号。它使用下一个补丁版本加 `-dev.<commit>`，例如
  `0.1.1-dev.a1b2c3d4e5f6`；
- 同一版本会注入 Go CLI、内嵌 Web UI、GUI bridge、移动端 Go 库，以及 Android 的
  `versionName`。Android `versionCode` 由 SemVer 单调映射生成。

本地构建也使用同一个入口：

```sh
make build SURFACE=terminal VERSION=1.2.3
make build SURFACE=web VERSION=1.2.3 OUTPUT_DIR=/tmp/pi-go-build
```

## 产物

| Surface | Release 产物 |
|---|---|
| `terminal` | Linux/macOS/Windows 的 amd64 与 arm64 压缩包 |
| `web` | 内嵌 Web UI 的 Linux/macOS/Windows amd64 与 arm64 压缩包 |
| `gui` | Linux amd64、Windows amd64、macOS amd64 与 arm64 原生包 |
| `mobile` | Android arm64 APK |

所有发布资产旁会生成 `SHA256SUMS`。正式 Release 使用 GitHub 自动生成的发布说明，并在说明开头
记录本次选择的 surface。

## 缓存

构建按照工具链分 job，缓存彼此隔离：

- Go module 和编译缓存按 runner、架构及对应 `go.sum` 区分；
- npm 下载缓存按各 frontend 的 lockfile 区分；
- Web 额外复用 Next 增量构建缓存，六个目标只执行一次前端生产构建；
- Android 缓存 Gradle 依赖与 wrapper、固定版本的 NDK 和 Wails CLI；Task 使用固定版本的预编译工具，
  避免为运行构建任务额外编译 Task 自身。

所有引用的第三方 GitHub Actions 都固定到完整 commit SHA，避免浮动 tag 在无代码变更时改变构建。

## 签名边界

当前项目构建入口生成的是可安装的 debug Android APK；它不适合直接提交应用商店。GUI 产物也尚未
进行 Windows 代码签名或 Apple Developer ID 签名、公证。GitHub Release、SemVer、版本注入和校验和
不依赖额外 secret；以后接入商店或桌面签名时，应在各自的原生 job 中配置证书，不把证书下沉到共享
Go Core 或通用构建 job。

如果 Release job 在创建 tag 时收到权限错误，请确认仓库 **Settings → Actions → General → Workflow
permissions** 允许 `GITHUB_TOKEN` 写入仓库内容。workflow 本身只在发布 job 请求 `contents: write`，
无需个人访问令牌。
