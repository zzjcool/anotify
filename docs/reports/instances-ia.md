# 实例区 IA：已卸载默认成组（#565）

## 判据

> 关注用户的主链路，用户的注意力有限。

上一版（#553）把已卸载实例藏在 `#pb-show-uninstalled` 开关后面：要知道有历史，得先猜到这个开关的存在；
而且没有历史时开关照样渲染 —— 按下去毫无反应，用户只能猜。本轮的判据是**用户不需要点任何控件就能看见它们**。

## 起点与基线口径

- 基线：`origin/main` = `2333ac8e`（旧 worktree 停在 `7b70b2f5`，已 reset）。
- 生效中的实例如的既有行为**不动**（对照组）：行结构、`data-release` / `data-digest` / `data-release-uninstall`
  契约逐字保留。

### 控件数基线（改动前，`web-src/pages/playbook.html` @ `2333ac8e`）

fixture 状态：`project-chain` 装了 `blue` / `green` 两个 release，然后把 `blue` 卸载
（= 1 个生效中 + 1 个已卸载）。实例卡片里的按钮数：

| 来源 | 个数 |
| --- | --- |
| `#pb-installs-head` 里的 `#pb-show-uninstalled`（`role="switch"`） | 1 |
| 生效中行（green）的 `[data-release-uninstall]` | 1 |
| 「查看 YAML」是 `<a class="btn">`，不是 `<button>` | 0 |
| **合计** | **2** |

“查看 YAML”锚点虽然视觉上是按钮，但 DOM 是 `<a>`，所以不计入 `<button>` 计数 —— 两轮的口径一致。

## 计划（本轮）

1. 删 `#pb-show-uninstalled`：模板里的 `role="switch"` 节点、`data-toggle="uninstalled"` 的 JS 分支、
   四份 locale 的 `pb.v4.show_uninstalled` 一起删。
2. 列表默认按「生效中 / 已卸载」分组：生效中在上；已卸载仅当存在时作为 `已卸载 · N 个` 分组出现。
3. 一次请求：进页面恒取 `?include=uninstalled`，不再为开关发第二次。
4. 超过 5 个 → 组内「展开其余 N 个」（一次性动作）。
5. 空态两种：从未装过（既有文案）/ 只有已卸载（“当前没有生效中的实例；下面是历史”）。

## 测量（待回填）

- 控件数对比：
- “不点就能看见”的证据：
- 变异两条：
- `make ci` / `make e2e-affected`：
