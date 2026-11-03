# MOTION / LOADING CONTRACT（UI v2 第二轮，2026-11）

> 事实源：4 路 scout 审计（2026-11-03）。目标不是"到处加动画"，而是补齐缺失的反馈、
> 统一节奏、把加载态从"骨架闪一下"变成可预期。

## 1. 动效词汇（只允许这些）

| token | 值 | 用途 |
|---|---|---|
| `--dur-fast` | 120ms | hover / press / 小元素淡入 |
| `--dur` | 200ms | 弹层 / toast / 抽屉 / 常规过渡 |
| `--dur-slow` | 320ms | 主题翻转遮罩 / 数字 count-up（JS 用 360ms） |
| `--ease` | `cubic-bezier(.2,.8,.2,1)` | 默认（进出都用它） |
| `--ease-out`（新增） | `cubic-bezier(.22,1,.36,1)` | 入场（列表/卡片/弹层） |

- 只动 `opacity / transform / filter / box-shadow / color / border-color`；**禁止新增**
  `width / height / max-height / top / margin` 过渡（现有侧栏 width 保留）。
- 禁止 `transition: all`；每个 transition 显式列属性。
- 列表 stagger：20ms/项，最多前 8 项；`animation-fill-mode: both`。
- 所有 CSS 动效必须走 token；`prefers-reduced-motion` 由现有全局 `*` 闸门 +
  `tokens.css` 的 `--dur-*:0` 覆盖。**JS 动画必须显式 matchMedia(reduce) 直接落终值**。

## 2. 红线（e2e 会红）

- **不要给这些元素加入场/位移动画**：`#sidebar`、`.app-nav`、`#app-shell`、
  `.page`、`#page-main`、`#scope-slot`。`ui_shift` 在 900–1500ms 窗口断言几何
  **只能有 1 个值**；`transform` 也会改 `getBoundingClientRect`。
- 列表/卡片入场只允许 `translateY(8px)`（不改 x、不改文档高度）。
- 动画不得改变点击目标中心；弹层 scale ≥0.98。
- 不碰 `scripts/e2e/**`、`internal/**`、`design/**`、`web/mock.js`。

## 3. 加载态规则

- 每个异步区域：`aria-busy="true"` + 骨架；数据/错误落地的**同一个同步块**里移除骨架与 `aria-busy`。
- 骨架高度与内容态误差 ≤24px；列表骨架行数 = 首屏常见行数（5）。
- 三态必须齐：loading / empty / error（error 带重试）。
- 骨架 shimmer 改**合成层**（伪元素 + transform），不再用 `background-position`。
- 详情页（node/source/playbook）保留 `min-height` 或等高骨架，CLS 目标 ≤0.05。
- 错误不得降级成空态（security）；未知值显示 `—`，不得先写死 `0`。

## 4. 文件归属（并行）

| 路 | 文件 | 内容 |
|---|---|---|
| A 动效 | `web/tokens.css`、`web/ui.css`、`web/partials.js`、`web-src/pages/index.html` | press、toast、modal、抽屉、主题遮罩、KPI count-up、列表入场、skeleton |
| B 加载 | `web-src/pages/{events,admin,security,node,source,playbook,trace}.html` | 骨架/错误态/高度预留；只用页内 `<style>`，不改 ui.css |
| 合并 | 先 A 后 B；B 若需共享类，报告"建议提升到 ui.css"，不自行改 |

locale 只允许 A 加 `ui.*` 键（4 语言）；B 用现有文案。

## 5. 验收

- 门禁：`check-classes` / `check-tailwind-css` / `sitegen` / `fe`。
- e2e：`frontend`、`ui_shift`、`lang_hint`、`i18n_coverage` 必跑；协调者最终跑全量。
- 视觉：每个动效给 3 帧（before/50%/after）或 30fps 序列；加载页给 loading/done 对照。
- 硬数字：`nav.y` 仍单值；index CLS ≤0.01；node/source/playbook CLS ≤0.05；toast/modal/drawer 有 ≥120ms 的可测过渡。
