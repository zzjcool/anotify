# v4 剩余任务 issue 交接

总任务：[[v4][EPIC] 剩余交付拆分与发布前完整验收](https://github.com/zzjcool/anotify/issues/330)

E1 b30e998 已独立验收。暂停后续源码实施，仅发布剩余任务；每项含目标、owner、scope、不做项、验收与依赖。

| 标识 | GitHub issue | 依赖 |
|---|---|---|
| V4_01 | [#331](https://github.com/zzjcool/anotify/issues/331) [v4][G3a] 装配 HTTP 资源 API 并保留独立认证与通知域 | E1 已验收基线 |
| V4_02 | [#332](https://github.com/zzjcool/anotify/issues/332) [v4][G3b] 打通 Node 受权产物上传、结果托管与私有下载 | #331 |
| V4_03 | [#333](https://github.com/zzjcool/anotify/issues/333) [v4][G3c] 落地 exact-five OpenAPI 与引用、鉴权、DTO 契约门禁 | #331、#332 |
| V4_04 | [#334](https://github.com/zzjcool/anotify/issues/334) [v4][G4a] 切换服务入口、开发播种与构建镜像工具链 | #331、#332、#333 |
| V4_05 | [#336](https://github.com/zzjcool/anotify/issues/336) [v4][G4b] 迁移非前端 E2E 底座并验证真实 Node 全链路 | #331、#332、#334 |
| V4_06 | [#335](https://github.com/zzjcool/anotify/issues/335) [v4][FE] 接入真实五 API 并校准失败 Task 与历史日志场景 | #331、#332、#333、#334、#336 |
| V4_07 | [#337](https://github.com/zzjcool/anotify/issues/337) [v4][HARDEN] 补强严格结构拒错与确定性安全回归 | E1 已验收基线 |
| V4_08 | [#338](https://github.com/zzjcool/anotify/issues/338) [v4][FINAL] 完成跨团队集成与发布前全量门禁 | #331、#332、#333、#334、#336、#335、#337 |

主线：#331 → #332 → #333 → #334 → #336 → #335 → #338；#337 可从当前冻结基线在隔离分支推进。先冻结真实 Node/harness 再由前端消费，没有循环依赖。

核心b30与前端bf21/1abc仍为本地交付，未push/merge默认分支；分派前提供共同集成commit/补丁，别重做完成部分。详 EPIC.md、MANIFEST.json、PUBLISH_STATE.json、published/真实远端正文。
