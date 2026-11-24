<!-- snapshot-doc: #565 交付时的定格数字（控件数对比、变异、门禁输出都是这次运行的实测；复跑以新输出为准） -->

# 实例区 IA：已卸载默认成组（#565）

## 判据

> 关注用户的主链路，用户的注意力有限。

#553 把已卸载实例放在 `#pb-show-uninstalled` 开关后面：要看见历史，得先猜到这个开关存在；而且没有历史时
开关照样渲染 —— 按下去毫无反应，用户只能猜。本轮判据是**有已卸载实例时，用户不需要点任何控件就能看见它们**。

## 起点

- 基线 `origin/main` = `2333ac8e`（旧 worktree 停在 `7b70b2f5`，已 reset）。
- 已卸载实例的**详情页**行为（#553 的已卸载视图 / 对象空态 / YAML 默认「安装时」）本轮不动，只改**列表区**。

## 落点

| 文件 | 改动 |
| --- | --- |
| `web-src/pages/playbook.html` | 删 `#pb-installs-head` 与 `#pb-show-uninstalled`（含 `data-toggle="uninstalled"` 的点击分支、`showUninstalled` 状态、`hasUninstalledOnly()` 的第二次请求）；`releaseRow()` 分「生效中 / 已卸载」两种行；新增 `uninstalledGroup()`（`已卸载 · N 个` 分组 + 5 行预览 + 「展开其余 N 个」）；`installs()` 恒取 `?include=uninstalled` 并按生效中 / 历史分组渲染；空态按「从未装过 / 只有已卸载」二分 |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | 删 `pb.v4.show_uninstalled`、`pb.v4.no_installs_uninstalled_only`；加 `pb.v4.no_active_installs`、`pb.v4.uninstalled_group`、`pb.v4.expand_rest`、`pb.v4.release_uninstalled_at`；`pb.v4.release_uninstalled` 去掉 `{at}`（时间单独一个字段，不再塞进徽章） |
| `scripts/e2e/fixtures/f3/f3-playbook.mjs` | `playbook-release` 用例：删开关的两态断言，换成「零点击可见」+ 控件数 + 一次请求 + 分组标题/顺序 + 6 个历史的展开 + 两种空态 |

样式上一行**没有新增 CSS 类**：已卸载行只是把行内链接从 `.field-label` 换成 `.field-hint`（本仓既有的
次要文字类，`--faint` 在卡片底色上仍 ≥4.5:1），徽章继续用 `.badge-warn`，分组标题用 `.card-sub`，
展开按钮用既有的 `.btn .btn-ghost .btn-sm`。

## 控件数对比（验收 2）

fixture 状态：`project-chain` 装了 `blue` / `green`，然后把 `blue` 卸载（1 生效中 + 1 已卸载）。
口径是实例卡片里的 `<button>` 个数（「查看 YAML」是 `<a class="btn">`，两轮都不计入）：

| | `#pb-show-uninstalled` | 生效中行的「卸载」 | 历史行的「卸载」 | 合计 |
| --- | --- | --- | --- | --- |
| 改动前（`2333ac8e`） | 1 | 1 | 0 | **2** |
| 改动后 | 0（节点不存在） | 1 | 0 | **1** |

**两次都是实测**，不是数源码：把页面文件切回 `origin/main` 的版本、其余（含 #565 的用例）不动，
跑 `./scripts/e2e/run_all.sh frontend`，用例里的同一个断言直接报出改动前的数：

```text
❌ playbook-release — AssertionError: #565 §baseline 量：实例卡片 <button> 总数（改动前应为 2；打印实际）
  2 !== 1
```

改动后同一条断言（`section:has(#pb-installs) button` 的总数 === 1）在
`playbook-release` 里通过 —— 也就是说：**开关没了，控件数从 2 降到 1**。

同一个「改动前」的页面还给了另一条更直接的证据：带着 #565 的用例跑，
`#pb-installs [data-release]` 等不到第 2 行（`page.waitForFunction: Timeout 4500ms exceeded`）——
改动前那一刻，已卸载的 blue 行**根本不在 DOM 里**，和用户点不点开关无关。

## 「不点就能看见」的证据（验收 1）

用例在 `goto(playbook)` 之后**一次点击都没有**就断言：

- `#pb-installs [data-group="uninstalled"] [data-release="blue"]` 存在且 `isVisible() === true`；
- 分组标题 = 字典里的 `pb.v4.uninstalled_group` 代入 `{n}=1`；
- `#pb-installs` 的直接子节点顺序 = `['green', 'uninstalled']`（生效中在上）；
- 已卸载行文本含：徽章 `Uninstalled`、这次卸载的时间（按 `installs?include=uninstalled` 返回的
  `uninstalledAt` 用浏览器 `toLocaleString()` 复算后逐字比对）、对象数、参数摘要（hover 全文）、
  「查看 YAML」深链带 `yaml=installed`。
- 同一页面只发**一次**实例清单请求：`/installs?` 的调用数增量 === 1（不再为开关发第二次）。

截图（套件产物，重跑 `frontend` 会重新生成）：`release-list-uninstalled-group.png`（1 生效中 + 1 已卸载的常态）、
`uninstalled-group-expand.png`（`hist-many` 展开后 6 行全在）。

## 空态（验收 5 的边界）

| 数据 | 渲染 |
| --- | --- |
| 从未装过（`hist-none`） | 既有文案 `pb.v4.no_installs`（逐字不变） |
| 只有已卸载（`hist-only`） | `pb.v4.no_active_installs`（“当前没有生效中的实例；下面是历史”）+ 下面的历史分组仍然列出那一行 |

## 超过 5 个（验收 4）

`hist-many` 预置 6 个已卸载 release：首屏渲染 5 行 + 「展开其余 1 个」；点一次后 6 行全在，按钮消失
（一次性动作，不改列表语义）。

## 变异

（下面两条各自单独打补丁跑同一套件，跑完立刻回滚；日志片段见各节。）

### ① 把分组藏回开关后面（默认关闭）

改动：`if (history.length) host.append(uninstalledGroup(history));` → 只在 `showUninstalled` 为真时才追加
（默认 false，等价于 #553 的默认态）。

结果：`playbook-release` 红，挂掉的正是验收 1 的那条：

```text
❌ playbook-release — AssertionError: #565 已卸载的 blue 默认就在列表里（一次点击都没有）
  0 !== 1
      at f3-playbook.mjs:448
```

### ② 已卸载行渲染「卸载」按钮

改动：`releaseRow()` 里 `if (!row.uninstalled) { …卸载按钮… }` → 去掉这个条件（历史行也拿到按钮）。

结果：`playbook-release` 红，挂掉的正是验收 3 的那条（它排在「控件数」聚合断言之前，所以是它先红）：

```text
❌ playbook-release — AssertionError: #565 已卸载行**不得**渲染卸载按钮（否则点了就是个 404）
  1 !== 0
      at f3-playbook.mjs:452
```

两次变异都是「改一处 → 跑整套 frontend → 立刻 `git checkout` 回滚」，回滚后套件恢复全绿。

## 门禁（本次实测输出）

| 门禁 | 结果 |
| --- | --- |
| `make ci` | **exit 0**（contract → check-agents → lint-go → fe → 各棘轮 → check-annotations → 构建 → `go test` → harness/锁自测，全绿） |
| `make e2e-affected` | **exit 0**：17/17 个请求套件达标，**断言 1709 通过 / 0 失败**，临时目录零残留 |
| ↳ 本轮真正相关的套件 | `playbook_release` 614 通过 / 0 失败（real）· `frontend` 46 通过 / 0 失败（real，`playbook-release` 在其中）· `i18n` 33 · `i18n_coverage` 44 · `ui_shift` 87（这四类各自遍历全页面，覆盖四份 locale 的键集/文案面） |
| `check-ui-copy` | ✅ 4 份 locale / 10072 条值 · 23 个页面 / 243 个文本节点 |
| `check-classes` | ✅ 889 个类全部落在设计系统 / Tailwind 工具类内（本轮没有新增类） |
| `check-i18n-keys` | ✅ 4 份 locale 键集合一致（本次删 2 个键、加 4 个键，四份同批） |
| 注释密度棘轮 | ✅ 与基线完全一致（没有上调；新增说明都落在既有注释行内或行尾） |
| `check-annotations` | ✅ unmarked 209 == 基线 209（新报告用 `snapshot-doc` 登记为定格数字） |
| `scripts/e2e/known-env-failures.json` | 未改动（没有新增已知环境失败；本轮没有出现需要它兜底的假红） |

复跑命令（本机）：`timeout 1500 make ci`、`timeout 2400 make e2e-affected`。

## 已知取舍

- 已卸载行的对象数来自服务端折叠视图，而它对**已软删**的对象「如实为空」（`internal/store/playbook.go`
  的注释就是这么写的），所以历史行显示 `0 能力 / 0 源`。这一行不是「从来就没有对象」的断言：徽章与
  卸载时间就在同一行上，要看清点进去的详情页有安装时快照。
- 历史分组内部沿用按 release 名字排序（与生效中一致），没有按卸载时间倒序 —— 本 issue 没要求，
  且排序口径统一才可复算。
