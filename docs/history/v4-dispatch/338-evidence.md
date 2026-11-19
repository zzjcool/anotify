# #338 最终门禁证据（2026-11-11）

> 迁移自 .pi-orchestrator/v4/issues/dispatch/338-evidence.md（2026-11-19，协调者工作区不再入库）
> snapshot-doc: 2026-11-19 迁移时的原件定格（文中数字是当时契约定格/实测，不是「现在是多少」的承诺）

> 最终集成身份：`v4/integration` @ **ee04fd72f93db8713ed98d006e2592c33f11f1fb**
> 版本串：`v0.2.0-346-gee04fd7`（anode `v0.2.0-346-gee04fd7+ee04fd7`）
> （初版证据为 d5c6a5d；rev-338 复核发现 e2e flake/F5-F8 后，追加 3 个修复分支并重新跑全部门禁）
> 工作树：`/root/code/wt-338`（branch `v4/integration`，`git status` 干净）；最终日志 `/root/tmp/338final4/`（74a75a5），历史日志 `/root/tmp/338final2/`（ee04fd7）

## 0. 复核修复（rev-338 ok:false → 收口）

- F2/F3/F4/F5：`fix/338-stability`（guest 登录真实就绪等待、i18n 布局稳定后测几何、套件预算 2×、exit≠0/0 断言/集合不符一律 fail、manifest 校验）+ 连带修 ux21 in-flight `/auth/me` 与 cli_auth AC-05 异步渲染。
- F8：`fix/338-security-gaps`（`additionalProperties`/`prefixItems` 的 writeOnly 打码 + 引用敏感性 + store/HTTP 验收）。
- F7：passkey 会话/registrationToken 一次性钉死，`TestEnrollE2E_CompleteReplay` 改名并做实断言。
- F6：`fix/338-repo-hygiene`（删除 203 个误入库运行产物/受忽略文件，保留 md 文档；gitignore 防复发）。
- 另：`fix/338-artifact-flake`（artifact GC 测试先 WaitBackground 再关库，-count=30 由 2 红转 30/30 绿）。
- 追加 `fix/338-rename-residual`（ff99993 + 06eafcf）：同名能力内 param 改名/删除时，actual 侧「当前包已不存在的 key」整值打码；空敏感集不再早退（rev-338c 抓到的 major，RED→GREEN：`rename-empty-keys-old-secret` → `***`）。

## 1. 机械门禁（全部真实 exit，日志 `/root/tmp/338final2/`）

| 门禁 | 命令 | 结果 | 日志 |
|---|---|---|---|
| 契约 | `make contract` | EXIT 0（A2/A4/A5/A6/A7/A8/A9/A11；exact-five 125 ops；228 schema 字段级） | `contract.log` |
| 单测 | `go test ./... -count=1` | EXIT 0（38 包） | `gotest-all.log` |
| 竞态 | `go test ./internal/store/... ./internal/ws/... ./internal/server/... -race -count=1` | EXIT 0（store 94s / v4test 500s / ws 25s / server 134s） | `race-core.log` |
| PR 门禁 | `make ci` | EXIT 0 | `ci.log`（part1 内） |
| 快速 e2e | `make e2e-quick` | EXIT 0（463 断言） | `e2e-quick.log` |
| 构建 | `make build` | EXIT 0（sitegen 4×19、指纹 30 资源/76 HTML） | `build.log` |
| 产物对账 | `make manifest` | EXIT 0：**稳定身份** = 版本串 `v0.2.0-336-gd5c6a5d` + spec `78a07f75…` + fe `d78ea6ba…`；anotify `149228ce…`、anode `5c22ea63…` 是**本轮构建（非稳定身份）**——二进制 sha256 随 `BuiltAt`/VCS stamp 变化（见 `docs/v4/e2e-inventory.md` §8「产物身份」） | `part1.log` |
| 节点 CLI | `make check-anode-skill` | EXIT 0（3 子命令/10 flag/版本 JSON/边界） | `part1.log` |
| **全量 e2e ×3** | `make e2e` 连续 3 次 | **3/3 EXIT 0：28/28 套件 / 2129 断言 / 0 失败 / TMP 零残留**（rev-338c 在最终 head 74a75a5 独立复跑，`/tmp/rev338c-rev/e2e-{1,2,3}.log`；此前 ee04fd7 亦 3/3 绿，`/root/tmp/338final2/`） | `e2e-1/2/3.log` |
| 镜像 | `make docker` + `docker run` `/health` | EXIT 0；`{"ok":true}`；镜像 36.1MB；容器已删除 | `docker.log` |

## 2. 全量 e2e 套件矩阵（28 = 24 real + 4 explicit-mock）

real：passkeys 7、deploy 93、orphans 98、api_contract 156、node_v4 88、live_agent 55、edge_cases 77、routing 168、persistence 82、contract 181、security 100、ws_protocol 134、lang_hint 86、cli_auth 133、passkey_enroll 85、admin_flow 52、deeplink 50、auth_flow 29、recovery_next 5、p2_guards 30、events_paging 68、password_login 75、capacity 82、**frontend 35**。

explicit-mock（`E2E_MOCK_ONLY=1`，裁决见 TRACKER 2026-11-11 13:5x）：i18n 29、i18n_coverage 40、ui_shift 83（matrix 304/304）、ux_regression_19_23 8。

authority manifest：`.e2e-bin/results/_authority-manifest.json`（每个套件 mode/authority/status/passed）。

## 3. FE 五套件

- `frontend` **real 35/35**（248 behavior assertions；含 pages-19 四语言真机页面、source HMAC、capability 真部署、playbook 回执打码）。
- 其余四个 explicit-mock 全绿（见上）；real 模式缺口已归类为 fixture 的 mock-only 假设（301/401/410/VAPID=off/无节点），不是产品 bug；真实 API/Node 由 frontend real + node_v4 + 13 HTTP + 7 legacy Chrome 承担。

## 4. 视觉验收（真实数据）

- 真实 35 case 截图 70 张：`…/screenshots/f3/runs/a5a28209-…/real/**`（capability 部署模态候选 `f3-deploy-target·旧 digest`、部署后 stale、playbook unapplied 打码、CAS 409 草稿保留、任务日志/artifact 410 等）。
- dark/light × desktop 1440/mobile 390 × 四语言矩阵截图：`…/screenshots/f3ui/runs/886e292d-…/real/**`（例：playbooks dark desktop、events dark mobile；无溢出、可滚到底、无 JS 错误；ui_shift 断言 304/304）。

## 5. 交付范围

- 本轮合并进 integration：PR #354(#335)、#358(#357)、#361(#349)、#363(#356)、#364(#362)、#365(legacy Chrome 7 套件 rebase)、#366(authority 具名映射 + real_backend 退役 + `docs/v4/e2e-inventory.md` + E2E_TESTING 修正)。
- 无 v3 兼容层/中央 executor/旧库迁移；`internal/executor` 不存在；旧库拒开不删数据（#337/#349 测试）。
- 已知残余（非阻塞，已记录）：#356 的 `additionalProperties`/`prefixItems` 与 `MaskValues` 同口径、同名能力内 param 改名无法归因；`capability.md` 与 `api/resource.yaml` 对 `observed` 的描述存在文档矛盾（实现遵 resource.yaml「恒 {}」）；`Deeplink`/`events_paging` 依赖 #335 页面已随合并转绿。
