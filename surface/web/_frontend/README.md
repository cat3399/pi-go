# pi-go Web UI

浏览器宿主使用 Next 与 PWA 注册，挂载 `surface/ui` 的共享 Workbench。
页面通过当前 origin 的 `/api/v1` 访问 command、query、snapshot 和一条全局 SSE。
产品组件、状态与样式位于共享 UI 包中。

## 开发与构建

从仓库根目录执行：

```sh
make setup SURFACE=web
make dev SURFACE=web ARGS='--cwd /path/to/project'
make check SURFACE=web
make build SURFACE=web
make run SURFACE=web ARGS='--cwd /path/to/project'
```

开发入口提供 Next HMR 和 Go API 自动重载。生产构建导出静态资源并内嵌到 `pi-go` 二进制，
Node 与 Next 只在开发和构建时使用。生成的 `out/` 由 Git 忽略；`_frontend` 的前导下划线使
Go 的递归包发现跳过 JavaScript 依赖。

通信与状态职责见 [Surface](../../../docs/SURFACES.md)。
