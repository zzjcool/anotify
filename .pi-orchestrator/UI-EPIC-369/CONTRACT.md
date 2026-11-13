# Epic #369 · 交付契约（冻结）

> 父 issue [#369](https://github.com/zzjcool/anotify/issues/369)；事实源 `.pi-orchestrator/UI-REVIEW-2026-11-13.md`。
> 本文件由协调者冻结。车道**不得**改本文件，也不得改别的车道的路径；需要扩权先上报协调者。
> 集成分支：`epic/369-ui-review`（**所有 PR 的 base**；`main` 在终局前不动）。

## 1. 通用硬约束

| 项 | 要求 |
|---|---|
| 基座 | 开工第一步 `git fetch origin && git rebase origin/epic/369-ui-review`，之后才读文档/改代码 |
| 写入范围 | 只写 §3 表里属于本车道的路径。跨车道文件（尤其 `web/ui.css`、`web/tokens.css`、`web/partials.js`）按 §3 的行号/区段约定 |
| 色值 | 不新增硬编码色值（`#hex` / `rgb(` / `hsl(`）；一律走 `tokens.css` 的 token |
| 类名纪律 | HTML 里的 class 必须来自 `ui.css`/`tokens.css` 组件类、页面内联 `<style>`，或 Tailwind 工具类；`node scripts/check-classes.mjs` 必须绿 |
| i18n | 新增文案必须四语言齐全（`zh-CN/en/ja/es`），键名一致；不允许只加 zh-CN |
| e2e 锚点 | **不得删**现有 e2e 定位器（`#id` / 文案锚点）。改文案/DOM 时必须同步 `scripts/e2e/suites/*` 里对应断言（改断言≠删断言） |
| 断言纪律 | 发现产品 bug 时**不改测试断言迁就**，写进 REPORT 的"发现的产品 bug" |
| 禁止 | 动 `.pi/settings.json`、`.env`、`.git/`、`node_modules/`、`.pi-orchestrator/UI-EPIC-369/CONTRACT.md`；不得 `sudo`；不得 `rm -rf` |
| 截图 | 落 `.pi-orchestrator/out/issue-<N>/`（gitignored，**不入库**）；PR body 里引用路径 |
| 提交信息 | `type(scope): 中文描述`，body 里 `Closes #<issue>` |

## 2. 门禁（每个 PR 交付前必须自己跑绿）

```bash
# 前端车道（全部前端 PR 都要）
make fe                                  # check-classes + sitegen + 指纹
node --test scripts/v4-frontend.test.mjs # 改了 web/partials.js 或 web/*.js 必跑

# Go / API 车道（只跑改动涉及的包，别跑 ./...）
go test ./internal/server/... ./internal/store/... -count=1
gofmt -l .
python3 scripts/check_openapi.py         # 改了 api/*.yaml 必跑
python3 scripts/check_contract.py        # 改了 api/*.yaml 必跑

# 与改动相关的 e2e 套件（单跑，别跑全量；全量由协调者每波跑）
./scripts/e2e/run_all.sh --serial frontend
```

### 已知的基线红（2026-11-13 在 `epic/369-ui-review` 上实测，**不是你引入的**）

`make ci` 在本机基线就红，**不要**把它当成车道门禁，也不要顺手去修（不在 #369 范围）：

| 位置 | 现象 | 性质 |
|---|---|---|
| `internal/sitegen` `TestDocsPageRendersFiveSpecFixturesFourLanguages` | `want 16 tag groups, got 0` | 确定性失败，main 上已有 |
| `cmd/anode` 5 个 `TestInstallScript*` | systemctl/sudo 交互断言 | 本机无 sudo，环境性 |
| `internal/ws` `TestRehelloUnknownBusyDoesNotRelease`、`cmd/anode/internal/runner` `TestRework2Timeout…` | 负载下偶发 | flaky，隔离单跑应绿 |

判断标准：**你的 PR 不能新增失败**。改了哪个包就跑哪个包；报告里贴命令与结果。

**端口分配（避免并行车道互相踩）**：`c-vis` 7781 / `c-fecontent` 7782 / `c-feactions` 7783 /
`c-mobile` 7784 / `c-ia` 7785 / `c-paging` 7786 / `c-copy` 7787 / 审查者 7790-7792 / 协调者 7780。

视觉自检（改样式的车道必做）：

```bash
NO_TUNNEL=1 MOCK_PORT=<本车道端口> ./scripts/mock.sh   # 后台起；用完 kill
# 用无头 Chromium（本机 Chrome 可用）在 1440×900 与 390×844 两档、dark/light 两主题逐页截图
# 断言：0 console error、0 pageerror、documentElement.scrollWidth == viewport 宽
```

## 3. 车道与文件所有权

| 车道 | issue | 可写路径 | 明确不可写 |
|---|---|---|---|
| `c-vis` | 370 371 372 373 | `web/tokens.css`；`web/ui.css` **全文唯一写入者**；#372/#373 允许改页面上的 class（只改 class 属性，不改结构） | `web/partials.js`、页面内联 `<style>`、`web-src/pages/*` 的结构/文案 |
| `c-fecontent` | 374 375 378 | `web-src/pages/{source,node,message,trace,events,nodes}.html`、`web/partials.js`（#378 期间唯一写入者）、`web-src/locales/*.yaml` | `web/ui.css`（新样式写页面内联 `<style>`）、`web/tokens.css` |
| `c-feactions` | 380 376 | `web-src/pages/{playbook,sources,source,security,playbooks,keys}.html`、`web-src/locales/*.yaml`、`web/ui.css` **仅** `/* — actions */` 标记段（见 §4） | `web/partials.js`、`web/tokens.css`、`c-fecontent` 的 6 个页面（`source.html` 只允许页头动作区） |
| `c-mobile` | 383 382 384 385 | `web/ui.css` **仅** 移动断点/响应式段（`@media` 块 + `.btn-sm`/`.copy-btn`/`.tab`/`.hm`/`.table-wrap`/`.flograph-*`/`.modal-*`）、`web-src/pages/{keys,admin,docs,index,events,capabilities}.html`、`web-src/layouts/*`、`web/flograph.js`、新增 `scripts/ui-audit-mobile.mjs`、`scripts/e2e/suites/*`（移动相关断言） | `web/partials.js`、`web/tokens.css` |
| `c-ia` | 381 | `web/partials.js`（NAV/`NAV_GROUP_OF`/`PUSH_UI_ENABLED` 区段）、`web-src/pages/{index,nodes,login,receivers}.html`、`web-src/locales/*.yaml` | `web/ui.css`、`web/tokens.css` |
| `c-paging` | 377 | `api/*.yaml`、`internal/server/**`、`internal/store/**`、`web-src/pages/{sources,playbooks,nodes,keys}.html`（只加搜索框/分页控件） | `web/ui.css`（分页控件样式走页面内联或现有组件类） |
| `c-copy` | 379 | `web-src/pages/{sources,playbooks,node,nodes,message,capabilities}.html`（文案/预设/模板区）、`web-src/locales/*.yaml` | `web/ui.css`、`web/tokens.css`、`web/partials.js` |

**并行冲突守则**：同一波里两个车道碰到同一文件时，以"先合并者的事实"为基准，后合并的车道 rebase 后必须**保留**对方已合并的改动（不许回退）。

## 4. `web/ui.css` 共享写法的唯一约定

`c-feactions` 只允许在 `ui.css` 里新增一段，必须**以这个标记开头**（放在 `.data-table` 段之后）：

```css
/* — epic-369-actions — 动作菜单/危险动作统一语言（#376）；本段归 c-feactions 独占 */
```

`c-mobile` 只允许：①新增 `@media` 块（追加在文件末尾，以 `/* — epic-369-mobile — */` 开头）；
②改 `.btn-sm`/`.copy-btn`/`.tab`/`.hm`/`.table-wrap`/`.flograph-*`/`.modal-*` 这几处的既有声明。

`c-vis` 是 `ui.css` 其它区域的唯一写入者（tokens、`.card`、`.badge-*`/`.status-*`、`.btn*`、`.tab`、重复定义清理）。
> `c-vis` 与 `c-mobile` 都可能碰 `.tab`：以 `c-vis` 的 #372 收编结果为准，`c-mobile` 的触控热区只能加 `::after`/`padding`，不改字号与配色。

## 5. PR 协议

```bash
git checkout -b ui/369-<slug>          # 例：ui/369-contrast
git add <显式路径>                     # 禁止 git add -A（会把 .pi-orchestrator 产物扫进去）
git commit -m "style(web): 容器层次与对比度达标（#370 #371）"
git push -u origin ui/369-<slug>
gh pr create --base epic/369-ui-review --head ui/369-<slug> \
  --title "style(web): …" --body "$(cat /tmp/pr-body.md)"
```

PR body 模板（缺一项就算没交付）：

```markdown
Closes #<N>

## 做了什么
- …

## 验收逐条
- [x] <issue 验收原文> → 证据：<grep 计数 / 对比度矩阵 / 截图路径>

## 自测
- `make fe` → PASS
- `./scripts/e2e/run_all.sh --serial frontend` → PASS (n checks)
- 视觉：`.pi-orchestrator/out/issue-<N>/…png`（1440/390 × dark/light）

## 遗留 / 发现的产品 bug
- （无则写"无"）
```

## 6. 上报格式（回给协调者）

```
DONE <issue 列表>
分支: ui/369-<slug>  PR: #<num>
产出文件: …
自测: make fe → PASS；e2e frontend → PASS(n)；截图 …
发现的产品 bug: …
遗留风险: …
```

失败/受阻就报 `BLOCKED <原因> + 已尝试 + 需要什么决策`，不要假装完成。
