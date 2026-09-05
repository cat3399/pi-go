# pi-go Mobile

Android 应用复用 `surface/ui` 的 React Workbench，通过 Wails WebView 与 Go 网络桥接访问远程
`pi-go web`。会话和 Agent 在服务器上运行，移动端模块不链接 Go Core。

## 工具链

- 设备：Android 8.0 / API 26 及以上，arm64-v8a。
- 构建：JDK 21、Android SDK API 35、Build Tools 35.0.0、Platform Tools、NDK 26.3.11579264。
- 环境：`JAVA_HOME` 指向 JDK，`ANDROID_HOME` 指向本机 Android SDK。

使用真机时启用 USB 调试即可，不需要模拟器或 Android 系统镜像。

## 配置与构建

从仓库根目录执行：

```sh
make setup SURFACE=mobile
make doctor SURFACE=mobile
make check SURFACE=mobile
make build SURFACE=mobile
```

产物为 `surface/mobile/bin/pi-go-mobile.apk`。默认构建 debug APK，使用
`~/.android/debug.keystore`。构建 release APK：

```sh
PI_GO_ANDROID_BUILD_TYPE=release make build SURFACE=mobile VERSION=1.2.3
```

APK 能否覆盖升级取决于签名证书是否一致。CI 固定证书与 Secret 配置见
[构建与发布](RELEASING.md)。

## 设备运行

连接已开启 USB 调试的设备后：

```sh
make devices SURFACE=mobile
make dev SURFACE=mobile
```

多设备时指定 adb serial：

```sh
DEVICE_ID=<adb-serial> make dev SURFACE=mobile
```

## 远程 Core

在运行 Agent 的电脑上启动 API：

```sh
PI_GO_WEB_PASSWORD='change-me' ./bin/pi-go web \
  --api-only --listen 0.0.0.0:30141 --cwd /path/to/project
```

在移动端连接页输入 HTTP 局域网地址或 HTTPS endpoint。
请求由 Go 网络桥接发送；HTTP 不依赖 WebView mixed content 设置，HTTPS 使用系统证书根，
SSE 断开后可以重连。协议见 [Surface](SURFACES.md)，源码归属见
[共享 UI 第三方说明](../surface/ui/THIRD_PARTY_NOTICES.md)。
