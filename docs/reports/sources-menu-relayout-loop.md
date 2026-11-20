<!-- snapshot-doc: 2026-11-20 #489 交付时的定格数字（真 Chrome 153 / Chromium 140 / make ci / e2e 的真实输出） -->
# 源页 ⋯ 菜单在减少动效下自激重排 → 页面冻死（#489）交付报告

> 分支 `pi-subagent/hw-nj/anotify-489-loop-b7a86641` · 基线 `origin/main` `b85c6d48`
> 复现浏览器 `google-chrome-stable 153.0.8010.52`（决定性变量）· 对照浏览器
> Playwright 自带 `Chromium 140.0.7339.16`
> 涉及面 `web/ui.css` + 两份页面脚本（`web-src/pages/sources.html`、
> `web-src/pages/source.html`，sitegen 产出 `web/*.html`）+ 用例 `scripts/e2e/suites/p2_guards.mjs`

一句话：**缺陷不是「动效太多」，是一条自己触发自己的回路 —— `placeMenu()` 写
`left/top` 产生 `transitionend`，`transitionend` 又驱动 `placeMenu()`。Chrome ≤152 把
它限速在 60 跳/秒（用户看到的是卡），Chrome ≥153 不再等帧，同一条回路变成同步死循环，
渲染主线程被自己的回调占满 ⇒ 页面冻死。**

---

## 1. 用户可见症状

Chrome/Edge ≥153 + 系统开了「减少动效」时：**打开源页面 → 点行尾的 ⋯ → 页面卡死**
（白屏/点了没反应）。本地 Chrome 153 与 CI 的 Chrome 157 复现，Playwright Chromium 140
不复现；4 路并发 + `CPUQuota=200%` 压测不复现 ⇒ 不是资源压力，是浏览器版本。

## 2. 机制（实证，不是推断）

下图的 `web/*.html` 行号是 **#489 取证时（修前）** 的：源在 `web-src/pages/*.html`，
`web/*.html` 是 sitegen 产物、不入库。

```
web/ui.css:4201-4209（reduce 兜底）   `*` 的 transition-duration: 0.001ms !important
                                      transition-property 仍是初始值 `all`
sources.html:250-271  placeMenu()     先清空、再写 menu.style.left/top
sources.html:290      transitionend → reflowMenus()
sources.html:273-276  reflowMenus() →  placeMenu()          ← 回到上面，闭合成环
```

每一次 `placeMenu` 都**一定**改变属性值（`left=""` → `left="812px"`），所以每一跳都
真的起一个过渡；duration 是 0.001ms，于是过渡立刻结束、立刻 `transitionend`，回路永远
不会「停下来」。

**消融实验**（同一浏览器 Chrome 153，只改一处）：

| 腿 | 做法 | 2s 内 transitionend | `page.evaluate` |
| --- | --- | --- | --- |
| A | 现状（reduce） | **≈1550**（prop = `left`/`top`） | **4001ms 超时（不返回）** |
| B | 只让页面不注册 `transitionend → reflowMenus` | — | 2ms ✅ |
| C | 只加 `.act-menu > .menu { transition: none }` | **0** | 3ms ✅ |
| D | 同一台浏览器但 `no-preference`（不减少动效） | 5 | 2ms ✅ |

B/C 两条腿都单独恢复了正常 ⇒ 回路确实经过「我们的 transitionend 监听器」，而且它的
能量来源就是「写 left/top 会起过渡」这一件事。

## 3. 改了什么（4 处）

| # | 文件:行（修后） | 改动 | 为什么 |
| --- | --- | --- | --- |
| 1 | `web/ui.css:2979-2996` | `.act-menu > .menu { … transition: none }` | 这个面板的位移由 JS 反复写 `left/top`，它**不需要**过渡（开合只切 `.hidden`，没有入场/退场动画）。`transition: none` 把 `transition-property` 设成 `none`，reduce 那条 `*` 规则只压 duration、不动 property ⇒ 面板写坐标**再也不可能**产生 `transitionend`。回路从源头断掉 |
| 2 | `web/ui.css:4241-4243` | reduce 块里 `.act-menu > .menu { transition: none !important }` | 兜底：将来有人给 `.menu` 加过渡，reduce 下也不会把回路接回去（与第 1 条同义，带 `!important` 提高必胜性） |
| 3 | 两份页面脚本的 `placeMenu()`（`web-src/pages/sources.html:262-300`、`web-src/pages/source.html:239-277`） | 加**幂等闸门** `MENU_POS`/`menuSig()`：记住上次落位时的输入（触发器矩形 + 视口 + 面板尺寸），一样就一次属性都不写 | 第二道闸，独立于 CSS：**位置没变的重排是零 DOM 变更**，所以「重排 → 产生过渡 → `transitionend` → 再重排」的链条在第二跳自己停在「没有变化」上 |
| 4 | 两份页面脚本的 `reflowMenus()` 与 `transitionend` 监听（`web-src/pages/sources.html:302-337`、`web-src/pages/source.html:279-314`） | ① 同栈重入保护 `REFLOWING`；② `transitionend` 里来自 `.act-menu` 内部（面板/触发器/条目）的事件一律忽略；③ 其余事件用 `requestAnimationFrame` 合并成一帧一次 | ①「自己触发的重排不许再进自己」；② 面板刚被我们写过 `left/top`，放行它的 `transitionend` 就是自激入口；③ 原来「一事件一次强制同步布局」，reduce 下页面上任何元素的一条过渡都会变成一次全量 `getBoundingClientRect`，按帧合并把上界压到 60 次/秒且不再随事件数增长 |

用例（不是产品代码）：`scripts/e2e/suites/p2_guards.mjs` 新增第 5 段
（`menuSelfReflowLeg` 在 `p2_guards.mjs:151`、段落入口在 `p2_guards.mjs:731`），
并在 `scripts/e2e/affected-map.mjs:203` 给 `p2_guards` 补上 `sources`/`source` 两个
页面（映射表的反向对账要求：套件源码里出现的页面必须登记，否则漏跑）。

只有 `web/**`（含 `web-src/pages` 两份页面源）与这一处用例/映射被改；
e2e 的其它门（预算、并行度、已知环境性失败清单、套件注册表）一个都没动。

## 4. 为什么之后自激**不可能**发生（不变式，不是「加了 guard 就不会」）

把回路写成一个状态机：`写坐标 → 过渡 → transitionend → 重排 → 写坐标 …`。要让它是
**无限同步**的，必须同时满足两件事：
(i) 每一跳都真的产生过渡；(ii) 每一跳都不需要等帧（Chrome ≥153 已满足）。

三道闸把 (i) 变成不可能，且**任何一道单独就够**：

1. **面板不参与过渡**（`transition: none` ⇒ `transition-property: none`）。
   过渡只可能由「属性的计算值发生变化」产生；`transition-property: none` 的元素的任何
   属性变化都不产生过渡 ⇒ `placeMenu` 写多少次 `left/top` 都产生不了 `transitionend`
   ⇒ 回路的「边」不存在。
2. **落位幂等**（输入相同 ⇒ 零写入）。假设将来有人把第 1 道闸拆了：那么第一次重排写坐标
   → 过渡 → `transitionend` → 重排；但这次重排的输入（触发器矩形、视口、面板尺寸）与
   刚刚落位时**逐字相同**，于是走闸门直接返回、一个属性都不写 ⇒ 不产生新过渡 ⇒ 链条
   长度上界是 **1**，不会自我维持。这与浏览器是否等帧无关 —— 有限跳数在任何速度下都
   只能花有限时间。
3. **触发侧收紧**（只认可能移动触发器的过渡 + 按帧合并 + 同栈重入保护）。
   即使第 1、2 道闸都被拆掉，页面上成片的过渡（reduce 下任何属性都 0.001ms）也只能
   让重排**每帧一次**，并且来自 `.act-menu` 内部的事件被丢弃 ⇒ 回路失去「自己喂自己」
   的那条边。

换句话说：Chrome 153 改的是 (ii)「回路有多快」，我们的修法拿走的是 (i)「回路存不存在」。
所以不依赖任何浏览器版本的阈值/启发式 —— 140 与 153 走同一条路径。

## 5. 先红后绿（同一套仪表，真 Chrome 153）

用例的测量口径写在 `p2_guards.mjs:60-124`：等页面安静（连续两帧无新事件且无在跑的 CSS
过渡）→ 计数器归零 → 点开 ⋯ → Node 侧 2s 采样窗 → 读回计数/几何（`page.evaluate`
2.5s 预算：冻死时它**永不**返回）。

| 断言 | 修复前（红） | 修复后（绿） |
| --- | --- | --- |
| `sources.html` 2s 内 transitionend < 10 | **页面冻死**：`page.evaluate` 2504ms 不返回，计数器读不出来 | **9 条**（全部来自触发器按钮自己的 `color`/`border-*`/`transform`），`page.evaluate` **4ms** |
| `sources.html` 菜单自身 left/top 过渡 = 0 | 读不出来（同一现场） | **0**（`left`/`top` 一次都没有） |
| `source.html` 2s 内 transitionend < 10 | **2126 条**（`left` 1021 + `top` 1021） | **4 条**（触发器 `color`/`background-color`/`transform×2`） |
| `source.html` 菜单自身 left/top 过渡 = 0 | 1021 / 1021 | **0** |
| 200 条外来 transitionend 不引发新过渡 | （该腿在冻死现场跑不到） | 新增**正好 200**、菜单位置不变 |
| Esc 关菜单 / 0 pageerror | — | 通过 |

同一段回路在 140 上的「修复前」读数也拿到了（见 §6）：`left`/`top` 各 120 条/2s，
正好 60 跳/秒 —— 与「140 只是把它限速在帧率上」的判断一致。

`sources.html` 修复前那 1550 条/2s 的原始读数来自独立探针（console IPC 计数，粒度 25）：
两次测量同向（探针 ≈1550+，`page.evaluate` 4001ms 超时；用例里直接冻死到读不出数）。
测量稳定性：修复后在 `sources.html` 上连跑 6 轮，计数 **9/9/9/9/9/9**、属性分布一致。

## 6. Chromium 140 回归（老浏览器不回归）

Playwright 自带 `Chromium 140.0.7339.16`，用**同一组断言**跑同一条腿：

| 项 | 修复前 | 修复后 |
| --- | --- | --- |
| 2s 内 transitionend | 249 条（`left` 120 + `top` 120 + 少量首屏颜色/`scrollbar-color`） | **8 条**（触发器自己的过渡） |
| 菜单自身 left/top | 120 / 120 | **0** |
| 菜单几何（视口内 / 贴触发器下方 8px / 不左溢） | 通过 | **通过（逐字相同）** |
| `page.evaluate` | 3ms | 3ms |
| Esc 关菜单 / 0 pageerror | 通过 | 通过 |

读法：140 上**可见行为没有任何变化**（菜单位置、开合、交互、DOM 都一样），变的是那
每秒 60 跳、用户看不见的空转重排被拿掉了 —— 这正是本次修复的目的。用例把几何与交互
作为「行为不变」的断言留下，所以将来谁把落位算错（例如把 §3 第 3 条改坏），140 这条腿
同样会红。

> 这条腿只在机器上装了 Playwright 自带 Chromium 时执行（`playwright-core` 不下载浏览器，
> CI 一般没有）。找不到时套件会**明说**「该腿未执行」，不伪造一条绿断言。

## 7. 门禁

- `flock /tmp/anotify-ci.lock -c "make ci"` → **exit 0**（含 `make fe` / `check-classes` /
  `check-css` / `check-affected-map` / `check-test-discipline` / `check-annotations` /
  `check-doc-links` / `check-secrets` / `go build ./...` / 全量单测 / 临时目录零残留）
- `make e2e-affected` → **exit 0**：`✅ E2E 全绿：29/29 个请求套件均达标（断言 2465 通过 / 0 失败）`，
  其中 `p2_guards 64通过/0失败 14.8s`、`frontend 43通过/0失败 206.9s`（#485 里那个
  挂满 52 秒被 SIGKILL 的 `source-actions` 在同一次全量里通过）、`ui_shift 87通过/0失败`。
  注：本次改到 `scripts/e2e/affected-map.mjs`，按映射表口径落在「共享面 ⇒ 保守跑全套」，
  所以这一跑就是全量 e2e（29 套）。
- `node scripts/e2e/affected.test.mjs` → 32/32 通过（映射表完整性/反向对账/死 glob）
- `node scripts/e2e/suites/p2_guards.mjs`（单独跑：真 Chrome 153 + Chromium 140）→ **64 通过 / 0 失败**
  （与并行编排器的计数一致：第 5 段在 3 条腿上共新增 19 条断言）

## 8. 为什么没选「改用 transform 定位」

`transform: translate3d()` 也是可过渡属性（`transition-property: all` 下同样会起过渡），
单独换属性**不能**断开回路；它还需要同时改 CSS 基线坐标（`left:8px` → `0`）、把现有
「祖先 transform 导致 fixed 包含块变化」的补偿算法一起改写 —— 改动面比收益大。本次
用「面板不参与过渡 + 落位幂等」，两者都是在**因果**层面断开回路，且 140 上的几何结果
与修复前逐字相同（§6 的几何断言）。

## 9. 遗留 / 已知边界

1. **`.act-menu > .menu` 现在完全不参与过渡**：如果以后要给这个面板加入场动画，需要
   把动画做在 `transform` 之外的容器上（或显式恢复 `transition-property` 并同时保留
   落位幂等闸门）。`web/ui.css` 那两条规则旁边的注释写了这件事。
2. **reduce 下「过渡变噪音」的普遍性没有根治**：`@media (prefers-reduced-motion: reduce)`
   只压 duration、不动 `transition-property: all`，所以页面上任何属性变化都会产生
   transitionend（本次观测到详情页首屏流程图一次渲染就甩出 80+ 条 `scrollbar-color`）。
   它现在不会再变成「重排风暴」（§3 第 4 条按帧合并 + 只认菜单外来源），但「reduce 下
   过渡本该整体关掉」是更大的话题，不在本 issue 范围。
3. 用例的采样窗是固定 2 秒 —— 这是**被测物理量本身**（每秒几次），不是「等就绪」的
   sleep；页面就绪仍然走 `waitForSelector` + `waitForTransitionQuiet` 的条件等待。
