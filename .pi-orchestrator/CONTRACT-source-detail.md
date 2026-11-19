# 冻结契约 · 源只读详情页（source-detail）

基线 `main @ b90bf5ef`，分支 `feat/source-detail-page`（不合 main、不推 main）。

## 路由与文件

- 新页 `web-src/pages/source-detail.html`，URL `source-detail.html?namespace=<ns>&name=<source>`（与 `capability.html` 同形；深链/刷新/前进后退由整页导航天然满足）。
- `web-src/pages/sources.html`：**名称链接**改指 `source-detail.html`（`a.src-title`，仍是 `data-name` 行的标题）；行内既有动作（编辑/立即运行/⋯）**保留**，编辑能力一点不丢。
- `web-src/pages/source.html`（现有编辑页）：**不改功能**；可加一个「查看详情」入口（可选）。
- `web/partials.js`：`loginHref().PAGES` 加 `source-detail.html`；`LIST_OF` 加 `source-detail.html → sources.html`。

## 页面结构（DOM 锚点冻结，e2e 依赖）

```
#src-detail[data-source="<name>"]          根
  #src-detail-back                         返回列表 → sources.html?namespace=<ns>&restore=1（可选 restore）
  #src-detail-name                         标题（displayName · name）
  #src-detail-edit                         编辑入口 → source.html?namespace=<ns>&name=<name>
  [data-sec="basic"]                       基本信息：kind / enabled / health / targetsResolved / lastFiredAt / 创建·更新时间
  [data-sec="config"]                      定义与字段：config（脱敏后）按现有 kv/JSON 渲染；payloadSchema 有就展示
  [data-sec="targets"]                     目标：targets[]（capability/version/params/expr 等按现有字段）
  [data-sec="recent"]                      近期回执：GET /v1/namespaces/{ns}/events?source=<name>&limit=20，无数据 → 诚实空态
  details.src-raw                          原始 YAML：默认收起，**展开才** GET /v1/namespaces/{ns}/sources/{name}/yaml
```

- 三态：加载骨架 / 缺 `name` 空态（带返回）/ 404 走 `loadErrorNode` + 返回。
- 复用既有组件：`.page-head`/`.card`/`.cap-kv` 风格 + `codeBlock()` + `copyText`；新类写在本页内联 `<style>`，颜色只用 tokens。

## 数据来源（全部既有端点，不新增）

| 块 | 端点 |
|---|---|
| 基本信息 / 定义 / targets | `GET …/sources/{name}`（读投影已脱敏：`hookUrl`/`secretSet`，无 token/secret 原文） |
| 近期回执 | `GET …/events?source=<name>&limit=20`（既有 `source` 过滤参数） |
| 原始 YAML | `GET …/sources/{name}/yaml`（上一轮已实现，复用，不造第二套） |

## 测试与同步清单

1. `scripts/e2e/fixtures/f3/f3-source.mjs`：列表点**名称** → `source-detail.html` 深链 200 + `#src-detail-name` + `details.src-raw` 展开后出现 `kind: Source` 且**不含** `token:`/`secret:`；再点 `#src-detail-edit` → `source.html`，原有编辑页断言（`#src-flow` 图、`#src-body`、`#src-yaml`、`#src-edges`）迁到编辑页那一段。
2. 页面清单 20→21：`f3-driver.mjs`、`f3-ui-helper.mjs`（`PAGES`/`ANCHORS`/`MATRIX_BATCHES` 上界）、`f3-cases.mjs`（`queryFor`/`pageAnchors`/`pages-21-*`/文案）、`f3-i18n-cases.mjs`（`anchors`）、`scripts/v4-frontend.test.mjs`（20→21、320→336、静态守卫若涉及）。
3. i18n 四语言补 `source_detail.*`（或沿用 `sources.v4.*` 命名，四份键集合一致）。
4. 服务端：**无新增端点**，所以无需改 `api/`；`make contract` 仍必须过。

## 门禁

`make fe`、`node scripts/check-classes.mjs`、`node scripts/check-i18n-keys.mjs`、`./scripts/e2e/run_all.sh --serial frontend`、`./scripts/e2e/run_all.sh i18n_coverage`、`node --test scripts/v4-frontend.test.mjs`（基线 9 条既有失败，不许新增）。
