# pi-go Mobile

`surface/mobile` is an independently built, remote-only mobile product. It
reuses the readable React Workbench from `surface/ui`, but it does not import or
link the pi-go Agent Core. Agent and session authority remain in the remote
`pi-go web` process.

The first supported target is Android:

- minimum Android version: Android 8.0 / API 26;
- build and target SDK: API 35;
- device architecture: arm64-v8a;
- transport: HTTP or HTTPS request/response plus reconnecting SSE;
- host: Wails v3 Android WebView with a small Go network bridge.

iOS, camera capture, push notifications, deep links, and background reminders
are intentionally outside this slice. No emulator or Android system image is
required; the development path targets a physical device.

## Toolchain

The configured macOS toolchain uses:

```text
JAVA_HOME=/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home
ANDROID_HOME=/opt/homebrew/share/android-commandlinetools
```

Required Android SDK packages are `platforms;android-35`,
`build-tools;35.0.0`, `platform-tools`, and `ndk;26.3.11579264`.

## Setup and checks

From the repository root:

```sh
make mobile-setup
make mobile-doctor
make mobile-check
```

Build a debug APK without installing it:

```sh
make mobile-build
```

The APK is written to `surface/mobile/bin/pi-go-mobile.apk`.

## Run on a physical Android device

Enable USB debugging, connect the device, and run:

```sh
make mobile-device-list
make mobile-run
```

When multiple devices are connected, select one explicitly:

```sh
DEVICE_ID=<adb-serial> make mobile-run
```

## Remote Core

Start the API on the computer that owns the Agent Core:

```sh
PI_GO_WEB_PASSWORD='change-me' ./bin/pi-go web \
  --api-only \
  --listen 0.0.0.0:30141 \
  --cwd /path/to/project
```

Open the mobile connection page and enter either an HTTP LAN endpoint such as
`http://192.168.1.10:30141` or an HTTPS endpoint. The mobile host performs
network requests outside the WebView, so HTTP support does not require enabling
mixed content and HTTPS uses the platform certificate roots.
