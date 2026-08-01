# Upstream baseline

记录日期：2026-08-01

## 主要上游

| 项目 | 仓库 | 基线 commit | 版本 | 作用 |
| --- | --- | --- | --- | --- |
| pi | https://github.com/cat3399/pi.git | a116523434806910336b9de3e38a41aa5860030b | packages 0.83.0 | 功能、行为和测试迁移的唯一主要基线 |

pi 的 commit 是首次迁移清单和 fixture 的事实起点，不代表 pi-go 永久冻结在该
版本。迁移目标是跟随 pi 的产品行为，同时使用符合 Go 习惯的实现。

## 下游参考

| 项目 | 仓库 | 参考 commit | 版本 | 作用 |
| --- | --- | --- | --- | --- |
| pi-web | https://github.com/cat3399/pi-web.git | dfab5853b8d2f717df259e7ebc94f49a3c2e43e7 | 0.8.6 | 未来外部集成的初始参考，不参与早期 core 设计 |

pi-web 不是 pi-go 的上游规范。记录该 commit 只为了保留项目启动时的背景；进入
外部集成阶段时，需要重新选择届时有效的 pi-web baseline 并重新分析，不能要求
pi-go 兼容这里冻结的内部实现。

## 更新 pi 基线

更新主要上游必须作为独立、可审查的同步变更，并同时：

1. 记录旧 commit、新 commit 和版本。
2. 生成源码、测试、依赖和持久化格式差异。
3. 分类新增、改变和删除的行为。
4. 更新 behavior ledger、test ledger、测试来源和历史 fixture。
5. 明确本轮已实现、暂缓、有意不同和不适用的项目。

只有清单内部一致、没有未分类差异时，才推进这里记录的 pi commit。

下游参考独立更新，不与 pi core 的 upstream sync 混在同一变更中。
