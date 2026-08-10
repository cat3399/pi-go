# WebUI 状态与差异

本文是 `pi-go-web` 的能力账本。视觉迁移、真实后端能力和验收证据分别记录；页面出现一个
控件不代表对应能力已经实现。

状态定义：

- `完整`：真实 Go production path 已接通，并有本地与大模块验收；
- `已接通`：真实实现可用，仍缺与原 pi/pi-web 的完整组合验收；
- `进行中`：存在真实代码，但模块尚未形成可用闭环；
- `未实现`：API 返回结构化 `501 capability_not_implemented`；尚未完成的 UI 禁用工作必须在
  下表单独列出，不能把可点击控件当成能力证据；
- `暂缓`：用户明确允许后置的插件、extension 等范围。

## 当前能力矩阵

| 模块 | 状态 | 当前事实 | 主要差异 |
|---|---|---|---|
| 视觉系统 | 完整 | 当前依赖闭包中的 67 个原 pi-web 源文件逐文件一致；`globals.css`、ChatMinimap 样式、主题、响应式布局、字体和静态资源原样进入静态构建，且用户已完成目视验收 | 构建边界所需的 manifest/config/type 解耦不属于样式变更 |
| 可选 Web 二进制 | 已接通 | `make web-build` 生成被忽略的 `surface/web/_frontend/out`，再由 `pi_go_webui` build tag 嵌入二进制；`make web-run` 不触发重建；目标统一委托 `scripts/webui.sh` | 还没有安装器和生产反向代理配置 |
| Web 开发循环 | 完整 | `make web-dev` 同时启动 Next HMR 与自动重载的 API-only Go；浏览器 `/api/*` 同源代理到 Go，不执行静态导出 | Go 进程重启会重新打开持久化 Session |
| Agent chat | 已接通 | `/api/agent/*` 直接 Dispatch Host，SSE 使用同一 canonical JSON projection；prompt/abort/queue/tools/model/thinking/compaction/bash/tree/fork/stats/name/reload 均走真实 Go owner | extension UI response/input 暂缓；更多断线、竞争与长历史组合验收仍需补充 |
| Session 列表/恢复/tree | 已接通 | Go Supervisor 支持并发打开去重、独立 Runtime、空闲回收、list/restore/detail/context/tree/state/rename 和 running IDs | delete、export、auto-name、按块延迟 thinking 读取尚未实现 |
| Models/配置 | 已接通（基础） | `/api/models` 读取真实 Go catalog/settings，new session 与 set_model 使用真实 Runtime/Host | auth 可用性过滤、models-config 编辑、provider 登录/API key 管理尚未实现 |
| 文件/Git/worktree | 未实现 | Go 文件工具与 shell 已存在 | Web 浏览、上传、diff API 尚未实现 |
| CWD/项目入口 | 已接通（基础） | home、cwd validate、默认工作目录和无项目资源执行时的 trust 状态为真实 Go API | directory picker、worktree switcher 和 project trust 持久管理尚未实现 |
| Surface capability contract | 已接通（Web） | `/api/capabilities` 与本表同步，未知 API 不回假数据 | 前端仍需按 capability 清单禁用 files/Git/plugins/skills 等可见入口；TUI 尚未迁移到同一 Supervisor |
| 插件/extension custom UI | 暂缓 | 不以假数据代替 | 完整 loader/交互未实现 |
| skills 搜索/安装 | 暂缓 | 核心 resource/skill 读取已有部分能力 | Web 管理模块未实现 |

## 已完成验收

- `npm run typecheck`、`npm run lint` 和静态 `npm run build`；
- API-only Go + Next HMR 的开发栈已通过同源 `/api/health`、models、自动 Go 重载和 SSE curl 验收；
- tagged production binary 已通过嵌入首页、缓存头、health 和独立 `run` 命令验收；
- 67 个复用的前端源码文件与 pi-web 基线 `a0668ab5077061a1bd074e11949e0a4b7974db2a`
  字节一致，CSS 哈希一致；
- 本地 fixture 贯通 `new session → SSE → prompt → AgentLoop → durable JSONL → context reload → fork identity rebind`；
- DeepSeek V4 Flash 短程只读验收真实调用 Go `read` 工具，并观察到
  `agent_start → tool_execution_start/end → agent_settled → prompt_done`。

## 明确未实现的 Web API

- files/file-index/upload/watch、Git status/diff、worktree 管理；
- models-config、provider auth/login/logout/API key 管理；
- session delete/export/auto-name/thinking block 按需读取；
- plugin/extension custom UI、skills 搜索/安装/更新；
- directory browser 与完整 project trust 管理。

这些路径当前只返回结构化 unsupported，不返回空 session、静态模型或演示内容。

## 验收原则

- CSS、组件结构、响应式布局、主题和资源已经由用户完成目视验收，不再重复安排浏览器视觉验收；
- 未实现能力必须至少在 capability API、本文件和结构化 API 错误中一致，后续再完成前端入口禁用；
- 小改动使用短程/fixture 测试，大模块完成后才运行 DeepSeek 真实只读任务；
- 真实验收必须能观察 Agent event、tool call、session JSONL 和最终回复，不只检查页面文本。
