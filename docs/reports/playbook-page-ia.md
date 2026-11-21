> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。
<!-- snapshot-doc: 2026-11-22 交付当次的门禁与断言数字（报告是定格记录）；复算命令逐条写在 §4 -->

# 预案详情页信息架构收口（#521）—— 交付报告

- Issue: [#521](https://github.com/zzjcool/anotify/issues/521)
- 分支: `pi-subagent/hw-nj/anotify-521-ui-ia-e3719b46`（基线 `main 584b0cd8`）
- PR: [#523](https://github.com/zzjcool/anotify/pull/523)（提交按 web / e2e / report 分开）
- 改动面: **纯前端**。`web-src/pages/playbook.html` + 四份 locale + 四个 e2e fixture 的操作步骤
- 不改: API / 后端 / store / schema / 任何字段

## 0. 结论

1. **一页只有一个主操作。** 页头一个「安装 / 升级」按钮开抽屉，抽屉里是「装一次要做完的全部输入」：
   值表单 → 实例名（release）→ 版本（默认当前）→ 主按钮；「渲染预览 / 试装」收进抽屉的「高级」折叠。
   装成功后抽屉自动收起，视线落回「实例列表里多了一组」；失败时抽屉不动（输入还在那里等着改）。
2. **其余都是状态展示。** 页面主体只剩三块：**实例列表**（按 release 分组 + 卸载）、
   **版本历史**（`<details>` 默认折叠）、**体检**（`<details>` 默认折叠，标题里给一行摘要）。
   模板编辑与删除收进另外两个默认折叠的进阶区 —— 它们不是安装流程的一部分。
3. **顶部一行状态**：`版本 v{n} · {n} 个实例 · 体检摘要`（+ 最近一次回执的入口）。
   它替代了原来那张「概要卡」，三件事分别由 `checkup()` / `versions()` / `installs()` 写入，
   整行每次重建，所以 `#pb-summary` 仍然"可被整体清空"——#380 的失败路径（清空 + `hidden` +
   空容器 0 高）逐字不变。
4. **所有既有 DOM id 一个没改**（`pb-release` / `btn-render` / `btn-dry-run` / `btn-install` /
   `pb-installs` / `pb-versions` / `pb-uninstall-result` / `pb-values` / `pb-conditions` /
   `pb-items` / `pb-unapplied` / `pb-receipt` / `pb-preview` / `pb-value-*` / `pb-file-*` …）。
   变的只有 e2e 的**操作步骤**（先开抽屉 / 先展开折叠），断言一条没删。
5. **"首屏看不到版本历史与体检明细"是可执行断言**，不是截图里的观感 —— 见 §3 的
   `playbook-install` 新断言（`details.open === false` 且明细 `isVisible() === false`）。

## 1. 目标信息架构

```
页头        标题 · 状态徽章 · 版本令牌        [返回] [刷新] [安装 / 升级]  ← 唯一主操作
状态行      版本 v2 · 3 个实例 · 体检通过      ← 一行事实，不占卡片
主体        实例列表                    （按 release 分组，每组带「卸载」，动作结果在它下面）
            ▸ 版本历史   2 个版本        （默认折叠：当前版本 + 每版指纹/时间）
            ▸ 体检       体检通过        （默认折叠：conditions / items / 未生效差异）
            ▸ 编辑 / 导出原始整包        （默认折叠：模板编辑，原样搬进去）
            ▸ 删除这份预案              （默认折叠）

抽屉      值（表单）→ 实例名（release）→ 版本（默认当前）→ [安装]
          ▸ 完整 values JSON 输入
          ▸ 高级：预览与试跑    [渲染预览] [试装] + 一句「都不自动部署」
```

两条"默认折叠"的例外都是**有事实要说话时自动展开**：

- **体检**在有「未生效差异」（`status.unapplied` 非空）时展开 —— 否则用户只看到一行绿字，
  点进去才发现 NOT ready。这条同时保住了 e2e 对 `#pb-unapplied` 的 `innerText` 断言
  （折叠容器的 `innerText` 是空串，断言会变成假绿/假红）。
- **安装动作的结果**（`#pb-action-state` / `#pb-preview` / `#pb-receipt`）平时 `hidden`，
  动作发生后才出现 —— 它是状态展示，不该在首屏占位。

## 2. 改了什么（按文件）

| 文件 | 改动 |
| --- | --- |
| `web-src/pages/playbook.html` | 页面结构重排：状态行 + 主操作按钮 + 三块折叠区；新增安装抽屉（`.modal-backdrop`/`.modal-panel` + 本页 `.pb-drawer*` 修饰，Esc / 焦点归还 / 点背景关闭都复用 `Anotify.openModal/closeModal`）；`versions()` 顺手填抽屉里的版本选择；版本历史每行的「按此版本安装」按钮删掉（装哪一版 = 抽屉里的一个选择） |
| `web-src/locales/{en,es,ja,zh-CN}.yaml` | 新增 10 个词条（键集四份一致）：`open_install` / `drawer_title` / `version_pick` / `advanced` / `checkup_ok` / `checkup_attention` / `checkup_incomplete` / `status_version` / `status_instances` / `versions_brief` |
| `scripts/e2e/fixtures/f3/f3-playbook.mjs` | 操作步骤：开抽屉 / 关抽屉 / 展开折叠的 helper；`install` 模式新增 #521 IA 断言（状态行三件事、抽屉默认关闭、版本历史与体检明细不可见、实例列表在首屏）与 360px 无横滚断言 |
| `scripts/e2e/fixtures/f3/f3-extra.mjs` | 同上（`matrix` / `historyMask` 两步先开抽屉 + 展开「高级」） |
| `scripts/e2e/fixtures/f3/f3-i18n-cases.mjs` | 同上；`scan()` 前额外把抽屉打开，让抽屉文案进「无残留 / 无裸 key」扫描面 |
| `scripts/e2e/fixtures/f3/{f3-cases,f3-ui-helper}.mjs` | 页面锚点 `#pb-file-path` → `#pb-installs`：模板编辑现在默认折叠，折叠容器里的锚点没有几何（`auditPage` 要求 `width/height > 0`）；`#pb-installs` 同样在 `#pb-content` 内、同样只有数据到达后才可见，锚点强度不变 |

**没有变的**：`installs()` 的分组/卸载逻辑、`receiptView()` 的回执渲染、`valuesForm()` 的字段生成、
模板编辑与删除的全部交互、`api()` 的请求形状（`version` 只在「装的是非当前版本」时才进 body，
渲染/试装仍按当前版本核对）。

## 3. 验收

### 3.1 `make ci`

`make ci` exit 0（含 `check-ui-copy`、`check-i18n-keys`、`check-classes`、`check-comment-density`、
`check-annotations`、`check-test-discipline`、`check-affected-map`、Go build/vet/test、基准冒烟）。

两处棘轮都是**等值**通过，没有上调任何基线：

- `check-comment-density`：`web-src/pages` 2400 行、`scripts/e2e/fixtures/f3` 1479 行，
  仓库总注释行 43335 —— 与基线逐字相同（新写的说明按同目录既有注释等量压缩，不新增预算）。
- `check-annotations`：`unbacked 298 / 298`、`unmarked 209 / 209`。

### 3.2 `make e2e-affected`

改动落在 `web-src/pages/playbook.html` + 四份 locale + f3 驱动面。选择器算出 **16 套**
（`docs/**` 那条改动被 `NO_E2E_IMPACT` 明确排除，日志打印了理由）：

```
✅ E2E 全绿：16/16 个请求套件均达标（断言 1070 通过 / 0 失败）· 退出码 0
   frontend 43 · i18n 33 · i18n_coverage 44 · ui_shift 87 · vars_ui 74 · lang_hint 86
   cli_auth 133 · passkey_enroll 85 · admin_flow 52 · deeplink 50 · auth_flow 49
   ux_regression_19_23 8 · recovery_next 16 · events_paging 179 · p2_guards 53 · password_login 78
```

`ui_shift` / `i18n` / `i18n_coverage` / `ux_regression_19_23` 按 #338 裁决跑 explicit-mock，
其余 12 套是 real authority；`known-env-failures.json` 保持空；无 case/timeout 重试。

另单独补跑**页面直接对应的 HTTP 面套件** `playbook_release`（release 身份 / 版本历史 /
卸载语义，真 server）：

```
./scripts/e2e/run_all.sh playbook_release → 225 断言 / 0 失败 · mode=real · 4.9s
```

### 3.3 "首屏看不到版本历史与体检明细"的证据

`playbook-install` case 里新增的一条断言（真 server + 真 Chrome，English 真身份）：

```
状态行三件事：版本 / 实例数 / 体检        （#pb-summary [data-status] 恰好 3 条且都非空）
抽屉默认关闭                              （#pb-install-drawer isVisible() === false）
版本历史默认折叠                          （#pb-versions-section.open === false）
版本历史明细不在首屏                      （#pb-versions isVisible() === false）
体检默认折叠                              （#pb-checkup-section.open === false）
体检明细不在首屏                          （#pb-conditions isVisible() === false）
实例列表在首屏                            （#pb-installs isVisible() === true 且文案非空）
360px 页面/抽屉都不横滚                    （documentElement.scrollWidth / 抽屉 scrollWidth ≤ innerWidth + 2）
```

截图（`d.shot`，与该断言同一次运行）：`first-screen-ia.png`（首屏：状态行 + 安装按钮 + 实例列表，
后三块是折叠的）、`drawer-360-no-hscroll.png`（360px 抽屉）、`pure-render.png`（抽屉 + 遮罩）。

### 3.4 回归覆盖（e2e 只改步骤）

点 `#btn-render` / `#btn-dry-run` / `#btn-install` / `#pb-value-project` / `#pb-values-json` /
`#pb-file-*` / `#pb-delete` 的用例全部仍然通过；改动只是「先开抽屉 / 先展开折叠」这一层：

- `playbook-install`：render 纯（响应无 `install` 键、零写）→ dryRun 零写入 → 两次真装 → 回执折叠/单次回执 → 手工部署 CTA
- `playbook-matrix` + `playbook-matrix-12`：12 格矩阵，未生效差异、原子冲突全 state 0
- `playbook-secret-history`：两轮 render/install，writeOnly 掩码递归透传
- `playbook-upload-edit` / `playbook-delete` / `playbook-errors` / `playbook-missing`：模板编辑、CAS 删除、
  断开重试、404 失败徽章（#380 的空容器契约逐字不变）

## 4. 复算命令

```
make ci
make e2e-affected
./scripts/e2e/run_all.sh playbook_release     # 页面对应的 HTTP 面套件
node scripts/e2e/suites/frontend.mjs --case playbook-install
```

## 5. 遗留与明确不做

- **不做**：安装语义 / release / 版本 / 卸载的 API 一字未动；不做路由级 redesign；不引入前端框架。
- 选择器没有把 `playbook_release` 选进来，是因为 `affected-map.mjs` 的「页面 → 套件」规则只读
  套件的 `pages` 字段，而 `playbook_release` 是用 `paths` 登记的页面（改表会把整张表推进
  `FULL_SURFACES`，超出本 issue 范围）—— 本轮改用「显式补跑」而不是改表，数字都记在 §3.2。
- 版本历史的「按此版本安装」按钮删掉了（装哪一版改为抽屉里的选择）：这是"一页只有一个主操作"的
  直接后果，`/versions` 的读面与 `install {version}` 的写面都没有变。
