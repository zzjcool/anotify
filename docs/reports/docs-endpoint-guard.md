<!-- snapshot-doc: 2026-11-19 #457 交付时的定格数字（make ci 的真实输出），复算命令见 §6 -->
# 接入文档端点漂移 + 文档端点检查（#457）交付报告

- Issue：[#457](https://github.com/zzjcool/anotify/issues/457)
- 分支：`pi-subagent/host-workspace/anotify-457-r3-aa45c4fb`
- 基线：`origin/main` @ `370fcd69`（分支已 rebase 到它；期间 main 新增了 `scripts/check-doc-links.mjs` / `scripts/check-ui-copy.mjs`，接线是并存的）
- PR：[#465](https://github.com/zzjcool/anotify/pull/465)（`Fixes #457`）

一句话：**接入文档教了两条不存在的端点**（`POST /v1/notify`、`POST /v1/events`），照抄必 404；
本轮把四份 locale 的接入文案换成 v4 事件入口，并把「文档提到的端点」变成对 `api/*.yaml` 的**外键**
（`scripts/check-doc-endpoints.mjs`，接进 `make ci`，变异自证会红）。

---

## 1. 现象与根因

`web-src/pages/docs.html` 的「接入 Agent」第 2 步是一条**可照抄的指令**：携带 Key 发 `POST /v1/notify`。
v4 里这条路由已经删了（`api/ingress.yaml` 对它 0 命中；`internal/server/api_integration_test.go`
的 `TestAPIIntegrationRouteTableIsV4Only` 明确钉住扁平路由必须 404），所以文档教的是一条必 404 的调用。
同类还有 `events.empty`（「用 `POST /v1/events` 提交第一个」）与 README 的 v3 端点表 / 示例 curl。

根因不是「写错了」，而是**文档里的 URL 没有执行者复算**：`check-i18n-keys` 只验 key 在不在、
`check-ui-copy` 只验文案有没有开发旁白、e2e 只验页面 200。端点名是自由文本，随时间只能变错。

## 2. 改了什么

| 文件 | 改动 |
| --- | --- |
| `web-src/locales/zh-CN.yaml`、`en.yaml`、`ja.yaml`、`es.yaml` | `docs.agent.step2.desc`、`docs.toc.events`、`events.empty` → `POST /v1/namespaces/{namespace}/events`（并按语言补上「namespace 换成你的命名空间」）；删掉只教死端点的死键 `docs.toc.notify`（全仓 0 引用） |
| `README.md`、`README.en.md` | v3 扁平端点表里三条死行标「**已删除** —— 改用 X」（`/v1/notify` → events 入口、`/v1/stream` → 节点流 / `GET /v1/console/events`、`/v1/notifications` → `GET /v1/console/events`）；Agent 上报示例换成 v4 事件入口的可跑 curl（与 docs 页同一份形状）；源码树注释 `api/  /v1/notify 上报` 改指向 `api/*.yaml` |
| `scripts/check-doc-endpoints.mjs` | 新增检查（见 §3） |
| `Makefile` | 新目标 `check-doc-endpoints`（先 `--self-test` 再体检），接进 `make ci` |
| `docs/lint-guards.md` | 登记 L10（判据要点 + 接线） |

口径：**v4 没有为通知单开端点**，通知语义已并入事件流 —— 所以 docs 页第 2 步教的是
「发一条事件」，不是「换个 URL 继续调通知接口」。

## 3. 检查设计（判据可自证）

事实源：`api/*.yaml` 的 `paths:` 键（五份 spec 合并去重，109 条）。文档里的每个 `/v1/<path>` 必须命中一条：

| 判据 | 内容 |
| --- | --- |
| 归一化 | 去 `?query` / `#fragment` / 行尾标点 / 尾斜杠；按 `/` 切段逐段比对 |
| 段级宽容 | spec 的 `{param}` 可被文档里的**具体值**命中（`/v1/hooks/src_xxx` 合法）；文档里的 `{ns}` / `:id` / `...` 也命中对应段。以斜杠结尾的基路径（`/v1/system/`）按前缀判定 |
| 历史提及 | 同一行（或上一行）写「已废弃 / 已删除 / 已下线 / 已退役」或 `deprecated` / `retired` / `removed` / `deleted` ⇒ 引用不存在算历史提及（计数不判红）；或整段包进 `<!-- docs-endpoints:historical -->` … `<!-- /docs-endpoints:historical -->`。**只对「引用不存在」生效**（端点存在时正常算解析成功）—— 沿用 `scripts/check-annotations.mjs` 的历史提及口子 |
| 遮罩 | `.html` 里 `<script>` 与 HTML 注释先遮罩：客户端代码/维护者注释不是教学文案 |
| 反假绿 | `spec 路径` / `扫描文件` / `引用数`三条下限（50 / 10 / 60），任一归零 ⇒ **退出码 2**；`paths:` 块里出现无法识别的行 ⇒ 退出码 2（fail-loud，不静默少算）；`--self-test` 24 条自证（红/绿双侧 + 行号 + 下限 + spec 解析 + 真实 `api/*.yaml` 能解析） |

## 4. 变异自证（本轮实测）

| 变异 | 结果 |
| --- | --- |
| `--self-test`：「已删除的 `/v1/notify`」「已下线的扁平 `/v1/events`」 | 红（`doc-endpoint-missing`） |
| 真实 locale 里写 `POST /v1/nonexistent-thing`，跑 `make check-doc-endpoints` | **红**：`1 处端点不存在于 api/*.yaml`，点名 `web-src/locales/en.yaml:1264`；`make` 退出非零 |
| 对**未修的 `origin/main`**（`--root=<main 的 worktree>`） | **红**：26 处（4 份 locale × 4 处 + 两份 README 各 5 处） |
| 修完的本分支 | 绿：109 条 spec 路径 · 32 个文档文件 · 102 处 `/v1/` 引用 · 历史提及 14 处 |
| `--root` 指向没有 `api/` 的空树 | 退出码 2（「事实源缺失 ⇒ 检查面为空，不是通过」） |

## 5. 检查面与已知盲区（有意为之）

- 扫面 = **sitegen 的输入面**（`web-src/pages`、`web-src/layouts`、`web-src/locales`）+ 两份 README。
  这是「用户最终看到的文档」的全集。
- **不扫** `docs/**`、`design/`、`DEVELOPMENT.md`、`IOS_TESTING.md`、`scripts/**`：它们是历史快照 /
  内部排障笔记 / 测试夹具，很多地方**故意**引用旧端点（v3 契约、复盘报告、`scripts/ws_test.mjs` 的旧探针）。
  把它们也管起来只会逼人到处贴豁免，最终得到「到处是豁免的假绿」。
- **不扫** `.html` 里的 `<script>` 与其它 `web/*.js`：那是客户端代码，URL 是拼出来的
  （`/v1/namespaces/{ns}/{plural}`），验它属于 e2e 的活。句柄命中的是**教学文案**。
- 已知盲区（登记在此，不假装覆盖）：`IOS_TESTING.md` 的 v3 示例 curl 仍是 `POST /v1/notify`
  （内部真机测试笔记，本轮不改，避免越出 #457 的范围）；`DEVELOPMENT.md` 里若干旧路径同理。

## 6. 门禁

```bash
flock /tmp/anotify-ci.lock -c "make ci"
```

结果：**PASS（exit 0）**，关键数字（`make ci` 真实输出，定格；运行于 `779f213f` + 本报告的工作区版本）：

- `check-doc-endpoints`：`--self-test` 24 条全绿；体检 `109 条 spec 路径 · 32 个文档文件 · 102 处 /v1/ 引用 · 历史提及 14 处`
- `check-annotations`：`unbacked 300 / 基线 300`、`unmarked 385 / 基线 385`（新增检查的文字按「检查」措辞，不涨棘轮）
- `check-i18n-keys`：4 份 locale 键集合一致，2189 个引用点全部有词条
- `check-ui-copy`：PASS（4 文件 / 9280 条值 / 20 条规则）
- `go test ./... -count=1 -shuffle=on`：PASS（41 个包 ok / 0 fail）
- `contract`（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + `cmd/anode` 可编译）：PASS

环境备注（诚实记录，不是本 PR 的产物）：本 worktree 的 `lint-go` 需要 `.bin/staticcheck`，首次运行时该目录为空且
`GOPROXY=direct` 取不到 `honnef.co`（网络受限）→ 从主 checkout 复制同一版本（`2025.1.1`）后重跑才绿；
`.bin/` 在 `.gitignore` 里，不进交付。

复算：`node scripts/check-doc-endpoints.mjs --self-test && node scripts/check-doc-endpoints.mjs`、
`node scripts/check-annotations.mjs`、`node scripts/check-i18n-keys.mjs`、`node scripts/check-ui-copy.mjs`。

## 7. 遗留风险

- `events.empty` 目前是**未被引用**的词条（文案已修正，但页面没有接线）——本轮只保证「不教死端点」，
  接线属于事件页文案的工作，不在 #457 范围。
- 检查只判「端点存在」，不判「方法对不对」：`GET /v1/namespaces/{ns}/events` 与 `POST` 同 path，
  文档写成 GET 也算绿。方法级核对需要 `paths:` 下的 method 键，属于下一步（未在本轮实现，避免判据变宽后误报）。
- `README` 的 v3 端点表仍是历史清单（逐行标了「已删除」），没有整表删除：它对读 v3 文档的人仍有对照价值，
  且现在每一行死端点都带「改用 X」。
