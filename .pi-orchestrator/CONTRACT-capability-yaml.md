# CONTRACT · 能力详情页脚本正文 + 资源原始 YAML（冻结）

as_of: 2026-11-17 · base: `e82dccff` · 协调者: 主 agent

## 0. 已确认的诊断（两个 worker 都**不要**重新侦察）

### A) 能力详情页看不到脚本 —— 根因：默认读面剥正文 + 详情面板从不懒加载

| 环节 | 事实 | 证据 |
|---|---|---|
| 存储 | **有**脚本。写入 `json.Marshal(artifact.Spec)`（`files` 无 omitempty），digest 覆盖 `Run` | `internal/store/capability.go:132-142`、`:230-262`；`internal/playbook` 安装路径 `internal/store/playbook_tx.go:321-380`；`internal/artifact/artifact.go:1113-1151` |
| 读 API | 默认**剥空** `spec.run.files` → `{}`；`?includeBody=true` 才回全文 | `internal/server/resource_capability.go:14-15`（设计注释）、`:578-591`（`capabilitySpecView` 把 `Run.Files` 置空）、`api/resource.yaml:56-67`（契约） |
| 前端 | 详情面板**复用列表行**：`loadCaps()` → `nsList("capabilities")`（只带 `?limit=500`）→ `renderDetail(cap)` 直接用同一对象，从不二次取数 | `web-src/pages/capabilities.html:1125-1149`（取数）、`:1355`（把列表行传进详情）、`:1616-1631`（`files` 为空 → `caps.detail.script_empty`）；`web/partials.js:1971-1974`（`nsList` 只拼 `?limit=500`） |
| 结论 | 不是「spec.files 为空」、不是「前端渲染逻辑坏了」，而是 **v3 的懒加载端点被删（v4 改成 `?includeBody`）后，详情面板没有跟着改造**；同时 `入口 run.sh` 仍在（只剥 files）→ 页面自相矛盾 | 页面注释 `web-src/pages/capabilities.html:1615` 写「v4 直接随资源返回」与契约相反，是错注释，要删/改 |

自测取证（已跑）：
- `go test ./internal/store -run TestCapabilitySpec -count=1` → ok（store 往返保留 `run.files`）
- `go test ./internal/server -run TestCapabilityResourceCreateAndGet -count=1` → PASS（默认 `{}`，`?includeBody=true` 回正文）

### B) 原始 YAML —— 只能「由存储的规范化定义重建」，拿不到用户原始字节

- `capability_versions.spec` / `sources.config,targets` 都是**归一化后**的结构化存储（默认值已补齐、键序固定）；用户最初贴的 YAML 原文只有 playbook 包 `playbooks.files` 里有。`internal/store/schema.sql:249-267`、`:599-617`
- 源的 `token`/`secret` **只写不读**：读面投影是 `hookUrl`/`secretSet`（`internal/server/resource_source.go:619-643`）。所以 YAML 里**绝不能**出现凭据明文（红线）。
- 现成序列化先例：`artifact.MarshalYAML`（JSON→YAML，保证 re-Parse digest 不变）`internal/artifact/artifact.go:1195-1218`

## 1. 冻结契约

### 1.1 新端点（后端 A 实现）

```
GET /v1/namespaces/{namespace}/capabilities/{name}/yaml   → 200 {"yaml": "<Capability YAML 全文>"}
GET /v1/namespaces/{namespace}/sources/{name}/yaml        → 200 {"yaml": "<Source YAML 全文>"}
```

- 响应 schema 名：`ResourceYAML`，`{type: object, additionalProperties: false, required: [yaml], properties: {yaml: {type: string}}}`。
  **故意不带 apiVersion/kind/metadata**（E1 信封规则只认「有 apiVersion + metadata」的 schema，避免把非信封响应伪装成信封）。
- `capabilities/{name}/yaml`：取该 name 的**最高版本**（与 `get` 同一套 `ListCapabilities`+`newestVersionOf`/或点查），序列化**完整** spec（含 `run.files` 全文、`params`、`next`、`requires`、`lifecycle`、`concurrency`）。
- `sources/{name}/yaml`：用**读投影**（`srcReadConfig` 的脱敏 config：`hookUrl`/`secretSet`，无 `token`/`secret`）重建文档；`kind`/`enabled`/`config`/`targets` 全带。
- `metadata` 里**不要**写 `namespace`（保持可整份贴回 playbook 包；包内禁止 namespace）。
- 错误语义与既有 `get` 对齐：未登录 401、跨命名空间 403、不存在 404（reason 码与 `get` 相同）。
- 两个路径都要加 `noStore` + `session` 中间件，与同族 GET 一致。

### 1.2 前端 A 修复（前端 B 实现）

- 打开能力详情时，懒加载 `GET /v1/namespaces/{ns}/capabilities/{name}?includeBody=true`，只替换 `[data-sec="run"]` 区块内容（**不要**整块重渲染，别丢展开态/滚动位置）。
- 加载中显示 `caps.detail.script_loading`，失败显示 `caps.detail.script_error`（不阻塞其它区块）。
- 同一能力只取一次（按 `name@version` 记忆），避免反复展开重复请求。
- 删掉 `web-src/pages/capabilities.html:1615` 那句与契约相反的注释。

### 1.3 原始 YAML 折叠块（前端 B 实现）

- 能力详情：在「版本历史」之前，加 `<details class="cap-raw"><summary>{{t "caps.detail.yaml"}}</summary>…</details>`；
  展开时（或用详情打开时）取 `/yaml`，用现有 `.code-block`/`.code-head`/`.copy-btn`/`.code-body` + `copyText(btn, getText)` 渲染，`<pre>` 必须 `overflow-x:auto`（长行不能被判溢出）。
  `.cap-raw`/`.cap-raw-hint` 的样式写在 capabilities.html 的**页面内联 `<style>`**（与既有 `.cap-*` 同处），只允许用 tokens 变量着色。
- 源详情：同理，放在「目标（targets）」之后、开发者信息之前；就地复用该页已有的 `.devf`/`.devf-hint`/`codeBlock()`（`web-src/pages/source.html:298-346`），key 走该页 `t()` 包装（`sources.v4.` 前缀）。
- 两个块都默认**收起**，展开才请求（懒加载）；失败显示 `*_yaml_error`，401/403/404 都要有兜底文案。
- 不得改动任何既有区块的 DOM 顺序/文案（e2e 有固定断言）。

### 1.4 i18n（4 份 locale 必须同时加，键集合要相等）

```
caps.detail.script_loading   caps.detail.script_error
caps.detail.yaml             caps.detail.yaml_hint
caps.detail.yaml_loading     caps.detail.yaml_error
sources.v4.yaml              sources.v4.yaml_hint
sources.v4.yaml_loading      sources.v4.yaml_error
```
（`web-src/locales/{zh-CN,en,ja,es}.yaml`；能力页 `t("caps.detail.…")` 直取；源页 `t("yaml")` 会拼成 `sources.v4.yaml`）

### 1.5 mock（前端 B 实现）

`web/mock.js` 必须实现两条 `/yaml` 路由（从 `state` 生成 YAML 文本，能力含 run.files；源用脱敏 config），
并让 `GET …/capabilities` 列表**默认剥 `run.files`**（跟真后端一致；否则 mock 永远测不出这个 bug）。
改完 `node --test scripts/v4-frontend.test.mjs` 必须全绿（若某条用例依赖旧的「列表带正文」行为，按真实契约修用例，不要改断言迁就）。

## 2. 文件所有权（互不越界）

后端 A：`internal/server/resource_capability.go`、`internal/server/resource_source.go`、`internal/server/openapi_parity_test.go`、`internal/server/resource_capability_test.go`、`internal/server/resource_source_test.go`、`api/resource.yaml`
前端 B：`web-src/pages/capabilities.html`、`web-src/pages/source.html`、`web/mock.js`、`web-src/locales/*.yaml`、`scripts/e2e/fixtures/f3/f3-capability.mjs`、`scripts/e2e/fixtures/f3/f3-source.mjs`

**禁止**：改 `Makefile`、`web/ui.css`、`schema.sql`、`.pi/settings.json`、任何测试禁用/跳过。
**不要**跑 `make fe` / `make sitegen`（生成物与 dist 由协调者统一重建），也不要 `git commit`（协调者统一提交）。

## 3. 各自门禁

后端 A：`go build ./...`、`make contract`、`go test ./internal/server/ -run 'TestOpenAPIParity|TestCapability|TestSource' -count=1`、`go test ./internal/store/... -count=1`
前端 B：`node scripts/check-classes.mjs`、`node scripts/check-i18n-keys.mjs`、`node --test scripts/v4-frontend.test.mjs`
