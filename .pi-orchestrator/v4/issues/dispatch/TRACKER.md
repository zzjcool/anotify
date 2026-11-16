# v4 剩余 issue 执行跟踪（协调者）

> 状态持久化：任何一步完成后更新本文件；上下文压缩后从这里恢复。
> 当前阶段：Round 1（#331 + #337 并行实现）。

## 分支/PR 模型

- 集成分支：`origin/v4/integration`（起点 `b30e998`，从旧 main 派生的 v4 全量交付线）。
- 每个 issue：`issue/<n>-<slug>` 从**最新 integration tip** 起 → 实现 centurion push → 协调者开 PR（base `v4/integration`）→ **独立 reviewer centurion** 审 PR → `ok:true` 后协调者 merge → integration 前进。
- 最终 #338 完成后：`v4/integration` → `main` 的发布 PR 交由用户决定是否合并（不经授权不发布）。

## 进度表

| Issue | 状态 | 分支 | PR | 实现 centurion | 审核 centurion | 备注 |
|---|---|---|---|---|---|---|
| #331 G3a HTTP 资源 API | ✅ 已合并（4 轮审核） | issue/331-http-resources @ 85ffa4a | #340 merged f309977 | cent-331/fix/fix2/fix3 | rev-331/331b/331d | 4 轮审核：r1 4major+14minor→修；r2 2major+2minor→修；r3 发现 #259 竞态→fix4；r4 ok:true（0 critical/0 major/4 minor） |
| #331-minors | 实现中 | fix/331-review-minors | - | cent-331-minors | - | m-1 确定性 WS 测试、m-2 取消哨兵、m-3/4 文档 |
| #332 G3b 产物传输 | ✅ 已合并 | issue/332-artifact-transport @ ebac915 | #342 merged 617d6ff | cent-332/332b | rev-332 | ok:true（4 minor）；60 新测试；D4 选择修复 |
| #333 G3c exact-five OpenAPI | ✅ 已合并 | issue/333-five-openapi @ 129355a | #343 merged fffb67e | cent-333/333b/333c | 内审 3 轮 + 独立复审后合并 | 124 op/228 schema；五 checker/parity；SAFE_BLOCKED→nodecap/sitegen |
| sitegen bridge | ✅ 已合并 | fix/sitegen-five-reader @ 0b3bb96 | #345 merged 85bc4aa | w-sitegen | rev-sitegen ok:true | 124 ops/5 specs；#334 的 make build 解锁 |
| #334 G4a 工具链/seed | 实现中（阶段A devseed） | issue/334-toolchain-seed | - | w-334a → 后续 Makefile/Docker | - | base 85bc4aa；devseed v4 重写中 |
| #335 FE 落地 | 阶段A 导入中 | issue/335-frontend | - | w-335a | - | 从 bf21f18 导入 web/web-src/5 套件；phase b 待 #336 |
| #335 FE 真实集成 | 等待 #331-334/#336 | issue/335-frontend-real | - | - | - | 需合并前端交付 bf21f18 |
| #337 HARDEN | ✅ 已合并 | issue/337-regression-hardening @ 2794b33 | #339 merged 43d80b1 | cent-337 | rev-337 | 独立审核 ok（0 critical/0 major/5 minor） |
| #338 FINAL 全量门禁 | 等待全部 | issue/338-final-gates | - | - | - | 发布 PR 到 main 需用户授权 |

## 已确认基线事实

- `b30e998` = E1 checkpoint；`internal/server` 引用已删除 `internal/executor`，`internal/api/events.go` 引用不存在的 trigger 类型 → 当前不能编译。
- server 95 个路由几乎全是 v3 扁平；唯一 namespaced 资源路由是 emissions。`internal/api` 只有 events.go，无 Resource 信封/CAS。
- `internal/resultspool` 不存在；`api/` 只有旧单文件 openapi.yaml（86 paths/109 ops）；sitegen 只支持单文件。五 spec、check_envelope 均未创建。
- store 侧资源 CRUD/CAS/manifest/upload 已具备；artifact download 有“首 expired 遮蔽后 live”缺陷（`internal/store/artifact_manifest.go:363-390`）。
- `#337` R1–R5 均已由 scout 用 file:line 证实。
- 前端交付分支 `bf21f18`（基于 ad21623，不含 core v4）需在 #335 时合入 integration。

## 门禁清单（每个 PR）

实现方：gofmt/diff-check/vet/指定包 test（-count=1，必要时 -race）/底层只读回归/真实 exit + log。
审核方：独立 checkout PR head、重跑全部验收命令、检查测试弱化与越界、检查证据真实性、`gh pr review --comment` 记录结论。
协调者：reviewer `ok:true` 后 merge；merge 后跑一次 integration tip 的快速冒烟（目标包 test）。

## 决策记录

- 2026-11-10：用户授权本轮提交 PR；旧文档「不 push/MR/merge」不再适用于本批 issue，但 main/发布仍需授权。
- 2026-11-10：PR base 采用 `v4/integration` 而非旧 main（v4 全量切换未发布）。
- 2026-11-10：#337 PR #339 已合并（merge commit `43d80b1`），integration tip 前进。
- 2026-11-10：#331 PR #340 审核结论 ok:false（4 major：保留读端点被删、upgrade 丢 up_to_date/force、logs noarchive 假空、console events ns 回显错误）。已退回 cent-331 返修，要求同分支追加 commit。
- 2026-11-10/11：#331 第四轮复审 ok:true，PR #340 已合并（merge `f309977`），integration 前进。4 个 minor 另开 `fix/331-review-minors` 补（m-1 WS 确定性回归、m-2 取消哨兵、m-3/4 文档）。
- 2026-11-11：#332（#342→617d6ff）、#333（#343→fffb67e）、sitegen 桥接（#345→85bc4aa）已合并；integration 已含 #331/#332/#333/#337 + sitegen。
- 2026-11-11：#334 阶段A（devseed v4 重写）与 #335 阶段A（bf21f18 前端产品导入）并行开工；#336 待 #334 的 build/anode。
- 2026-11-11：herdr server 不可用，新 centurion 终端启动失败；改用 in-process worker + 手工 worktree（/root/code/wt-*）继续，审核仍由不同 agent 完成。
- 2026-11-10：发现 base 5fcb95a 的 server 包仍有 #259 类收尾竞态（`/root/tmp/332/base-pkg-run2.log`，负载下 ~50%）；已派 cent-331-fix3 做结构性修复（fixture 级 settle/seal，5 次全包+负载验证）。#331 暂不合并，等 fix4。
- 2026-11-10：前端交付分支 bf21f18 基于 ad21623（不含 core v4），并含 web-src/sitegen/e2e 五套件改动；计划在 #335 开工前由协调者合入 integration 并解冲突。

## 2026-11-11 08:30 UTC 快照（供上下文压缩后补水）

- integration tip：`0b0501b`（#331/#332/#333/#334/#336/#337 + bugfix #348/#352 + gate #351 均已合并）。
- **#336 已合并**（PR #353 @ 8d4e2c2→0b0501b）：13 个非前端套件 1321 断言两连绿；node_v4 真 anode 88。
- **#335 进行中**：分支 `issue/335-frontend`，real-mode 迭代中。已完成 mock 五套件 195/0、ui_shift 300s、real bootstrap（prod ns/双会话/真实资源）、4 个 FE 契约修复（stats/nodeId/effectiveVars/dry-run）→ real frontend 6/35。当前 worker `w-335f` 修 204/节点槽位/登录 301/预期 4xx 白名单，剩余 observe-*/playbook-* mock 事实型断言与 receivers 设备卡。
- **#338 待办**：`make ci` 已由 #351 修绿；13 非 FE e2e 已绿；待 #335 合并后跑全量 `make e2e`（含 10 个 legacy Chrome 套件，可能需迁移/具名退役），然后开 release PR `v4/integration → main`；#329 届时 close 为 superseded。
- #349 follow-up：tasks.stop_intent（running abandon+人话 reason 已知限制）。
- 死记录：cent-331-minors/rev-332/w-bugfix/w-336b 会话已结束但 terminal 记录无法 retire（herdr server 曾崩，忽略其 stall notice）。
- 证据：`/root/tmp/336final/gate-*.log`、`/root/tmp/rev336*`、`/root/tmp/335b`、`/root/tmp/rev335/REVIEW-348.md`。

## 2026-11-11 13:0x UTC 快照（Round 2：3 centurion 并行）

- 会话 01a1c60c 接续（原会话在 w-335h 收口时被打断）。已完成：wt-335b 的 WIP 已提交 push（head `8645ff3`，mock 35/35、v4test 205/0、real 32/35），PR #354 base 从 `main` 修正为 `v4/integration`。
- 新 issue：#356（InstallReceipt unapplied 泄漏 writeOnly values，P0 安全）、#357（anode SIGKILL 回收确认竞态 + 物理槽位假占 + 误报日志复制失败，发现于 `go test ./...` 门禁）。
- 同时在跑（≤3 centurion）：
  - `cent-335`：wt-335b，修 real 三例（A: #355 HMAC 真分支；B: deploy 节点候选；C: 依赖 #356）+ 四个 FE suite real 模式。卡：`dispatch/335-close.md`。
  - `cent-356`：wt-356，`internal/playbook` 回执打码；卡：`dispatch/356-mask.md`。
  - `cent-349`：wt-349，tasks.stop_intent；卡：`dispatch/349.md`。
  - `w-357`（worker，不计 centurion 上限）：wt-357，runner 回收竞态；复现：12 CPU burner + `-count=60` 必现。
- #338 协调者侧基线（wt-338 @ 66d926a 实跑）：
  - `make ci` 通过（含 contract 125 ops/228 schema、fe、vet、单测、bench、harness selftest）。
  - `make e2e-quick` 通过（463 断言 0 失败）。
  - `make build` 通过（v0.2.0-298-g66d926a，30 资源/76 HTML）。
  - `go test ./... -count=1` **红**：仅 `cmd/anode/internal/runner` 的 `TestOldLeaseACKDoesNotExtendNewAttempt`（= #357）。日志 `/root/tmp/338/gotest.log`、`ci.log`、`e2e-quick.log`、`build.log`。
- #331/#332/#333/#334/#336/#337 关闭审计（scout）：实现/测试/PR 合并均齐；缺口为运行时门禁证据（最终 #338 留档）、#336 e2e inventory 文档缺失、`scripts/e2e/suites/real_backend.mjs` 已具名退役但文件仍在、`E2E_TESTING.md:167` 陈旧。→ 并入 #338 收尾。
- 下一轮（本轮完成后）：合并 #354/#356/#349/#357 + `338/chrome-v4`（3651707，7 个 legacy Chrome 套件迁移，527/0）→ 最终 `go test ./...`/`make contract`/`e2e-quick`/`ci`/`build`/manifest/全量 `make e2e` → 前端真机截图验收 → 关 issue #331-#337 + #338 + epic #330 → release PR `v4/integration → main`（合并需用户授权）。

## 2026-11-11 13:5x 裁决：五个 FE suite 的验收模式

- `frontend`：**real**（真实 server/anode/devseed），35/35；`playbook-secret-history` 依赖 #356。
- `i18n` / `i18n_coverage` / `ui_shift` / `ux_regression_19_23`：**explicit mock**（`E2E_MOCK_ONLY=1`）各完整一次；证据记录 `mode: explicit-mock` / `authority: explicit-mock`。
- 理由：四套件验证 locale/DOM/几何/交互契约，不含真实 API 语义；real 模式缺 session/seed bootstrap（`guest index redirected to password login` 等系统性失败）属新底座工作，不在 #335 范围。真实 API/Node 覆盖由 `frontend`(real) + `node_v4` + 13 个 HTTP 套件 + 7 个迁移后的 legacy Chrome 套件承担。
- #338 落实：`scripts/e2e/parallel_runner.mjs` 对这四套件显式注入 `E2E_MOCK_ONLY=1`（具名映射 + 注释，不静默 fallback），并在最终证据文档列出真实/显式 mock 的覆盖分工。
- 该裁决不影响 #338 的 `make e2e` 全绿定义：全部套件仍真实执行、真实 exit，只是上述四套件的 authority 明确为 explicit-mock。

## 2026-11-11 14:0x 更新：#356 交付切换

- 原 PR #359（`issue/356-receipt-secret-mask` @ 0824c47）关闭为 superseded：经 cent-356 独立 review + 实证探针，仍有 2 处明文泄漏（dual-type `items` 抢 `properties`；actual 同名 target 顺序颠倒按位置继承），结构错位 fail-open、缺 store/HTTP 读侧验收测试。
- 现交付：**PR #360**（`issue/356-receipt-secret-mask-hardened` @ 209e3d2），父已独立跑 `go test ./internal/playbook/... ./internal/store/v4test/... ./internal/server/... -count=1` EXIT=0、gofmt 空、vet 0；10 条具名回归（含两轮安装、嵌套/祖先/双类型、actual 对齐、store 落库+读回、HTTP 读侧、render/digest 不变）。
- 待办：#356 需要**另一个独立 centurion** 做 PR 审核（cent-356 是作者方，cent-356b 独立复核中可作候选）。
- #357：PR #358（`fix/357-runner-reap-race` @ 24c5783），待独立 centurion 审核。

## 2026-11-11 15:2x 快照（Round 3）

已合并进 `v4/integration`：
- `1e65e2e` PR #358 = #357（anode 回收竞态；rev-357 独立审核 ok:true）。
- `2e4bb48` PR #361 = #349（tasks.stop_intent；rev-349 独立审核 ok:true，3 minor）。

开放 PR / 分支：
- **#356**：PR #363 `issue/356-receipt-secret-mask-cent356b` @ `c4a22e8`（含整块模板、祖先继承、actual 同名并集、dual-type properties 优先；10+ 具名测试，父独立跑 playbook/store/server EXIT=0）。原 #359/#360 均 superseded 关闭。待 `rev-356` 独立审核。
- **#362**（新，产品 bug）：`capability.status.nodes` 只查当前版本行 → `stale` 零赋值、部署候选恒空。`w-362` 在 `wt-362` 实现（store 按名查询 + deployedState 三态），待 PR。
- **#335**：PR #354 head `84dee2d`，real 33/35；剩 `capability-actions`（依赖 #362 合并 + fixture 预部署 v2）与 `playbook-secret-history`（依赖 #356 合并）。mock 五套件全绿（35/35×2、29/29、40/40、83/83+304/304、8/8）。
- **#338 底座**：`w-338prep` 分支 `fix/338-gate-authority` @ `9d43f39`（四套件显式 mock 注入 + authority manifest + 删除 real_backend.mjs + docs/v4/e2e-inventory.md + E2E_TESTING 修正），已验证 i18n 299/0、ux 13/0，待 PR/审核。
- **legacy Chrome**：`338/chrome-v4` @ 3651707 **rebase 到 2e4bb48** → `wt-chrome2` @ `6de6037`（8 文件：7 套件 + lib/v4.mjs）。rebase 后实测 5/7 绿（cli_auth 133/0、passkey_enroll 85/0、admin_flow 52/0、auth_flow 29/0、password_login 75/0）；`deeplink` 38/12（详情页落“消息不存在”）、`events_paging` 30/1（selectOption 超时）。`w-chrome2` 正在修，分支 `fix/338-legacy-chrome-v4`。
- **重要教训**：上一轮「527/0」是对 **integration 的旧 v3 套件** 或旧的 `.e2e-bin` 二进制测的；`338/chrome-v4` 的 v4 迁移提交未合并前，integration 上的 6 个 legacy 套件必然 404。所有 e2e 结论必须 `rm -rf .e2e-bin` 重建后取得。

待办（顺序）：
1. rev-356 通过 → 合并 #363；关 #360（已关）。
2. w-362 → PR + 独立 reviewer → 合并。
3. #335 复跑 real 35/35（merge integration）→ 独立 reviewer 审 #354 → 合并。
4. w-chrome2 两套件绿 → PR（legacy 迁移）→ 审核/合并。
5. 合并 `fix/338-gate-authority` → 跑 `338-gates.sh`（contract / go test ./... / race / ci / e2e-quick / build / manifest / anode-skill / **全量 make e2e**）。
6. 前端真机截图验收（dark/light、桌面/手机、四语言、滚到底、无 JS 错误）→ 关 #331-#337 + #338 + #330 → release PR `v4/integration → main`（合并需用户授权）。

## 2026-11-11 17:3x 收口（Round 4）

集成身份：`v4/integration` @ `d5c6a5d`（v0.2.0-336-gd5c6a5d）。本轮合并：#354(#335)、#358(#357)、#361(#349)、#363(#356)、#364(#362)、#365(legacy Chrome)、#366(authority/退役/文档)。

最终门禁（完全独立 worktree /root/code/wt-338，日志 /root/tmp/338final）：
- contract、go test ./...、-race(store/ws/server)、make ci、e2e-quick、build、manifest、check-anode-skill 全 EXIT 0；
- **make e2e：28 套件 / 2129 断言 / 0 失败 / 285.2s / TMP 零残留**（24 real + 4 explicit-mock，authority manifest 落盘）；
- docker build + 容器 /health `{"ok":true}`（36.1MB，容器已回收）。
- FE：frontend real 35/35；其余四套件 explicit-mock 全绿；真实截图 70 张 + dark/light × desktop/mobile × 四语言矩阵。

已关闭：#331 #332 #333 #334 #335 #336 #337 #349 #356 #357 #362。发布 PR **#367**（v4/integration → main，159 commits/990 files）已创建，待 rev-338 独立审核后请用户授权 merge；#338/#330 在审核通过后关闭。
证据文档：`.pi-orchestrator/v4/issues/dispatch/338-evidence.md`。

## 2026-11-11 19:2x 复核收口（Round 5）

rev-338 对发布 PR #367（head d5c6a5d）判 **ok:false**：e2e 3/3 flake（F2/F3/F4）、判分口子（F5）、产物入库（F6）、passkey 覆盖不实（F7）、additionalProperties.writeOnly 缺口（F8）。

收口分支（均已合并进 v4/integration）：
- `fix/338-stability`（4418453）：guest 登录真实就绪、i18n 几何稳定等待、套件预算 2×、exit≠0/0 断言/集合/manifest 一律 fail；连带 ux21 in-flight `/auth/me`、cli_auth AC-05 异步渲染。
- `fix/338-security-gaps`（ec859b3）：additionalProperties/prefixItems writeOnly 打码 + 引用敏感性 + store/HTTP 验收；passkey 会话/registrationToken 一次性钉死，CompleteReplay 改名做实。
- `fix/338-repo-hygiene`（6e7c8e0）：删 203 个误入库运行产物（gz/log/patch/txt/json + .pi-legion/contract.md），保留 210 个 md 文档；gitignore 防复发；m9/m11 文档。
- `fix/338-artifact-flake`（72e5c20）：artifact GC 测试先 WaitBackground 再关库（-count=30 2红→30/30绿）。

最终 head：`ee04fd7`（v0.2.0-346-gee04fd7）。**全部门禁重跑**（/root/tmp/338final2）：contract/gotest/ci/build/manifest/docker 全 0；**make e2e ×3 连续全绿（28/28 套件、2129 断言、0 失败、284.6/285.6/285.6s、TMP 零残留）**；docker /health `{"ok":true}`。
待 rev-338b 复审 → 通过后关 #338/#330，请用户授权 merge #367（v4/integration → main）。

## 2026-11-11 20:1x 全部关闭

rev-338b 对发布 PR #367（head ee04fd7）判 **ok:true**：独立 3× make e2e 全绿（28/28、2129/2129/2130、0 失败）、F1-F8 逐条关闭、contract/gotest/ci/build/manifest/docker 全 0。
#338、#330 已关闭；GitHub 无 open issue。**发布 PR #367（v4/integration → main）待用户明确授权 merge**（其余工作已完成）。

## 2026-11-12 02:5x 最终（含改名残余收口）

- 用户裁决：改名多打码先修；旧库破坏性升级接受（开发阶段无其他用户）；observed 本轮不做；真机验收用户最后做；发布物（tag/镜像/GitHub Release）暂缓。
- `fix/338-rename-residual`：`ff99993`（knownParams：改名/删除的旧 key 整值打码）+ `06eafcf`（空敏感集不早退；rev-338c 抓到的 major，RED→GREEN）。
- 最终 head：`74a75a5`（v0.2.0-348-g74a75a5）。rev-338c 增量复审 **ok:true**，独立 3× e2e 全绿（28/28、2129 断言、0 失败、TMP 零残留）；上一轮 rev-338b 对 ee04fd7 也已 ok:true。
- PR #367 保持 OPEN，**未 merge / 未打 tag / 未推镜像**（按用户"发布物先不管"）。
