# web/fonts — 自托管字体说明

本目录是站点全部字体的唯一来源，**不依赖任何 CDN**。页面只通过
`/fonts/fonts.css` 引入（三个 layout：`base` / `focus` / `login`），
外部字体 CDN 的 `preconnect` 与外链已删除（全文不再出现外链标签）。

- 引入方式：`<link rel="stylesheet" href="/fonts/fonts.css" />`
- 全部 `@font-face` 都带 `font-display: swap`，字体未就绪时先用兜底栈渲染，不阻塞首屏。
- 文件名会被 `scripts/hash.mjs` 指纹化（dist 里是 `name.<hash8>.woff2`），
  该脚本会同步改写 `fonts.css` 里的 `url(...)`；
  `scripts/gen-icons.mjs` 直接读 `web/fonts/sacramento.woff2` 渲染图标，改名会同时影响它。

## 文件清单

| 文件 | 体积 | 字体 | 字重/形态 | 子集 |
| --- | ---: | --- | --- | --- |
| `inter-latin.woff2` | 46.9 KiB | Inter | `wght 300..700`（变量字体） | latin |
| `inter-latin-ext.woff2` | 110.2 KiB | Inter | `wght 300..700` | latin-ext |
| `fraunces-latin.woff2` | 63.3 KiB | Fraunces | `opsz 9..144`, `wght 340..420`（变量字体） | latin |
| `fraunces-latin-ext.woff2` | 91.8 KiB | Fraunces | 同上 | latin-ext |
| `fraunces-italic-latin.woff2` | 41.2 KiB | Fraunces Italic | `opsz 9..144`，单字重 400 | latin |
| `fraunces-italic-latin-ext.woff2` | 36.7 KiB | Fraunces Italic | 同上 | latin-ext |
| `jetbrains-mono-latin.woff2` | 29.7 KiB | JetBrains Mono | `wght 400..500`（变量字体） | latin |
| `jetbrains-mono-latin-ext.woff2` | 34.8 KiB | JetBrains Mono | 同上 | latin-ext |
| `sacramento.woff2` | 23.9 KiB | Sacramento | 400 | 全字符（无 unicode-range） |
| `greatvibes.woff2` | 36.6 KiB | Great Vibes | 400 | 全字符（无 unicode-range） |
| `caveat.woff2` | 39.6 KiB | Caveat | 400 | 全字符（无 unicode-range） |

首屏实际下载量：中英文页面只会命中 **latin** —— 正文页一份 Inter
（46.9 KiB），用到 `.font-display`（登录页 Fraunces）或多页面 mono 元素
（docs 等）时再加 63.3 / 29.7 KiB；四份 latin 全命中才是 181.1 KiB，
不是单页成本。`latin-ext`（四项合计 273.5 KiB）只在页面出现欧洲扩展区
字符时由浏览器按 `unicode-range` 触发下载，zh/en/ja/es 内容实测 0 命中。
CJK 不走自托管（体积不可控），由 `tokens.css` 的 `--font-sans` 兜底到
`PingFang SC` / `Microsoft YaHei` / 系统 sans。

## @font-face 映射（`fonts.css`）

- `Inter` → `inter-latin*.woff2`，`font-weight: 300 700`，`font-style: normal`
  （旧的 `Inter:wght@300;400;500;600;700` 请求由这一份变量字体等价覆盖，
  轴上 300/400/500/600/700 全部按原字重渲染，不再按字重重复下载）
- `Fraunces` → `fraunces-latin*.woff2`，`font-weight: 340 420`，`font-style: normal`
  （`opsz` 轴由浏览器 `font-optical-sizing: auto` 自动插值；页面用的 600 会 clamp 到 420）
- `Fraunces` italic → `fraunces-italic-latin*.woff2`，`font-weight: 400`，`font-style: italic`
- `JetBrains Mono` → `jetbrains-mono-latin*.woff2`，`font-weight: 400 500`
- `Sacramento` / `Great Vibes` / `Caveat` → 原自托管文件，400
- `unicode-range` 与 Google 下发**逐字一致**（含 latin-ext 块里与 latin 重叠的
  `U+0304/U+0308/U+0329`；重叠码位按 CSS「后声明者胜出」，因此「基字母 + 组合
  变音符」的组合可能额外触发一次 latin-ext 下载，代价 110.2 KiB，当前内容 0 命中）

两处已知的、由契约冻结的取舍：
- JetBrains Mono 只到 `wght 500`：docs 页 `.method` / `.doc-h2-code` /
  `.doc-step-num` 用的是 600，会 clamp 到 500（**不会**掉回 monospace 兜底，
  实测 600 与 500 等宽、均 ≠ monospace）。要恢复 600 需按 `wght@400..600`
  重新下发并把 `fonts.css` 的 `font-weight` 改成 `400 600`。
- Fraunces italic 两个文件目前**无页面实际使用**（`.auth-title em` 被
  `login.html` 内联样式改成 `font-style: normal`），保留是因为契约要求
  italic 400 可用；不用时浏览器不会请求（实测 6 页均 unloaded）。

## 来源与许可证

全部字体的许可证均为 **SIL Open Font License 1.1**
（<https://openfontlicense.org>），允许自托管、打包与再分发；
字体文件不得单独售卖，若修改字体需沿用 OFL 发布（本项目未做字形修改，
仅按 Google Fonts 下发的 woff2 子集原样存储）。

| 字体 | 上游 | 版权 | 下载来源 |
| --- | --- | --- | --- |
| Inter | <https://github.com/rsms/inter> | © The Inter Project Authors | `https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap` |
| Fraunces | <https://github.com/undercasetype/Fraunces> | © The Fraunces Project Authors | `https://fonts.googleapis.com/css2?family=Fraunces:ital,opsz,wght@0,9..144,340;0,9..144,420;1,9..144,400&display=swap` |
| JetBrains Mono | <https://github.com/JetBrains/JetBrainsMono> | © The JetBrains Mono Project Authors | `https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&display=swap` |
| Sacramento | <https://fonts.google.com/specimen/Sacramento> | © The Sacramento Project Authors | 仓库历史文件（早期自托管，来源同为 Google Fonts） |
| Great Vibes | <https://fonts.google.com/specimen/Great+Vibes> | © The Great Vibes Project Authors | 同上 |
| Caveat | <https://fonts.google.com/specimen/Caveat> | © The Caveat Project Authors | 同上 |

> 旧的 layout 曾把 Inter/Fraunces（以及 docs 页的 JetBrains Mono）指向
> `fonts.googleapis.com`，导致离线/内网部署时字体静默降级；现在全部本地化。

## 刷新字体（升级版本时）

必须用 **Chrome UA** 请求 CSS2 接口，否则返回的是旧格式/非变量字体：

```bash
UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 \
(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

curl -sS -A "$UA" \
  "https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700\
&family=Fraunces:ital,opsz,wght@0,9..144,340;0,9..144,420;1,9..144,400\
&family=JetBrains+Mono:wght@400;500&display=swap" -o /tmp/gf.css
```

然后按 `/* latin */`、`/* latin-ext */` 注释块抓出 `src: url(...)` 的 woff2
（每族每子集只有一个 URL —— Google 的 VF 文件与字重无关），下载落盘到本目录，
并把对应的 `unicode-range` 原样写回 `fonts.css`。落盘命名约定
`<family>[-<style>]-<subset>.woff2`（如 `fraunces-italic-latin.woff2`）。
校验下载正确性：同族同子集的 5 个字重 URL 应当**完全相同**（VF 一份文件
覆盖全部字重）；若不同说明 UA 被识别成不支持变量字体，需换 Chrome UA 重试。

校验清单（改完必须跑）：

1. `node scripts/check-classes.mjs && node scripts/check-tailwind-css.mjs && make sitegen && make fe`
2. `go run ./cmd/fe-preview -dir web -addr 127.0.0.1:7811`，
   浏览器 DevTools Network 里不应出现 `fonts.googleapis.com` / `fonts.gstatic.com`；
   `document.fonts.check('16px Inter')` / `('300 16px Inter')` /
   `('16px "JetBrains Mono"')`（mono 页）应为 `true`；不同字重的实测宽度应
   互不相同（300 < 400 < … < 700），相同说明 VF 轴没生效。
3. `./scripts/e2e/run_all.sh`（至少 `frontend` / `ui_shift` / `lang_hint` 三套）。
