# Epic #369 · 待派发车道卡（模板）

> 协调者用。派发时把对应 issue 卡整段交给 centurion，并补上"开工/rebase/门禁/PR 协议"三件套（见 CONTRACT）。
> 端口：c-err 7785 / c-ia 7786 / c-mobile2 7784 / c-copy 7787 / c-emph 7788。

## `c-err` → #378 错误态分级（partials.js 是它独占）

- `web/partials.js:2437-2480` `loadErrorNode` 增加 401/403/404/409/410/5xx 分支：401 跳登录；403 说"你没有权限 + 找谁（管理员/命名空间所有者）"；404 说不存在；409 说冲突并给「重新加载」；410 说已过期；5xx 说服务端出错 + 重试。**不再出现裸 HTTP 码**。
- `web/partials.js:2109-2145` 错误 toast → **持久条**（手动关闭 + 复制详情），成功仍可自动消失。
- 原生 `confirm()`（17 处）/`alert()`（3 处）收敛成统一危险弹层 + 删除类「已撤销（5s）」；`grep -c "confirm(" web-src/pages` 目标 0。
- `openModal` 绑 Esc（`:2489-2512`）。
- 归属：`web/partials.js` 独占；页面侧 `confirm()` 迁移除 `sources/security/playbooks/keys`（#376 已改）外的所有页面。
- e2e：新增 403 页级断言；现有错误态断言不失效。

## `c-ia` → #381（已冻结决策 B：下线「通知接收」+ 首跑闭环）

- **B 决策**：删 `web-src/pages/receivers.html`；清 `web/partials.js:2036` 白名单、`login.html:1229` redirect 白名单；docs 注明"手机推送未开放"；i18n 键保留不删（减少无谓 diff），在 REPORT 里列成遗留。
- 首页给「第一条事件」三步路径（源 → 能力 → 节点），替代单点「接入新 Agent」（`index.html:360-376`）。
- `nodes.html:96-110` 示例命令引用**当前实例真实存在**的能力，或给"先建一个能力"的链接。
- 恢复码弹窗加「我已保存」确认（与 API Key 页一致）。
- 实例无用户时登录页默认落「注册」tab。
- e2e：新增首跑路径用例（无用户 → 注册 → 首页 → 三步引导可见）。
- 归属：`web/partials.js` 的 NAV 区段 + `index/nodes/login` + locales；**不要与 #378 同时改 partials.js**（排队）。

## `c-mobile2` → #384 主内容不再必须横滚（→ 之后接 #385）

- 390/360/768 三档：keys 表（2.01×）/admin 表（1.77×）<640 改行式或 3 列+展开；链路图 <640 只画 3 节点 + "还有 N →"（或纵向）；热力图 <640 折叠成一行摘要；`pre.code-body` <640 `pre-wrap` 或折叠+复制（docs 最高 2.67×）。
- 保留交互式横滚时必须给可见提示（渐隐+箭头），不嵌套双层滚动。
- e2e：把 `.overflow-x-auto`/`.code-body`/`pre` 的全局豁免改成"白名单 + 白名单内也断言有滚动提示"；新增 390 视口用例。
- 工具：复用 `scripts/ui-audit-mobile.mjs`（clip-aware 版本）+ `--checks overflow`；ui.css 只写 `/* — epic-369-mobile — */` 段。
- #385（接在同车道）：index 区块顺序（事件流提到热力图前）、events 筛选默认收起、keys 副标题瘦身、modal 滚动体与底栏加渐隐/内距、`viewport-fit=cover` + `env(safe-area-inset-*)` + `-webkit-tap-highlight-color`、新建能力改向导（与 #379 模板接口配合）。

## `c-copy` → #379 文案与未接能力

- 4 处设计旁白从首屏移除（`sources/playbooks/node/nodes` 的 `.page-sub`/卡片说明）→ 移入 docs 或 `title`/`aria-description`。
- `message.html` 相位徽章只显示一种语言（现在「成功 · succeeded」）。
- `sources.html` 源编辑器加 cron 预设按钮（写进 `#f-expr`），引用 `sources.preset.*` 四个 i18n 键。
- `capabilities.html` 的 `#cap-modal` 接 `/v1/capability-templates`：选模板 → 预填 params/next/脚本，必填字段 ≤5，不需手写 JSON Schema。
- i18n 四语言同步。
- 归属：`sources/playbooks/node/nodes/message/capabilities` 的文案与新建弹层；**与 #376 的 sources 行结构冲突时保留对方改动**。

## `c-emph` → #373 强调密度收口（最后做，等 #372 合入）

- 逐页把 `btn-primary` 降到 ≤1（security 8 → 1 等），其余降 `btn`/`btn-ghost`；14 处 JS 动态生成的逐个改。
- 首页 spark/delta 强调面收敛（`index.html` KPI 与 `.stat-spark`/`stat-delta`）；空 spark 虚线去掉（处方 P0-2）。
- security 首屏强调面 ≤3。
- 交付逐页 `grep -c` 表 + security/capabilities/login 三页改造前后截图。
- 归属：所有页面的 class 属性（不改结构）；`index.html` 的 `.stat-spark` 区段；`ui.css` 不改（#372 已收编）。
