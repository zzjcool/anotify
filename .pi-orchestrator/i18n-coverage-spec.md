# i18n 覆盖补全 · 实施规范（协调者定，三批 frontend 统一遵守）

> 目标：非中文语言（en/ja/es）下，用户可见文案 **0 条中文残留**（演示数据除外，见 §4）。
> 事实源：本文件。三批 frontend 顺序执行，各自验收后交接。

## 1. key 命名规范

- 页面级：`{page}.{section}.{语义名}`（page = index/keys/receivers/security/message/login/docs/common）
- 共享运行时（partials.js）：`common.{语义名}`
- 语义名用 snake_case，见名知义：`index.greeting.late_night`、`common.time.just_now`
- **带变量的文案**：值里用 `{var}` 占位，JS 侧用字符串 replace（partials.js 无插值层，不引新库）：

  ```yaml
  index.recent_summary: "最近收录 {total} 条通知，其中 {ok} 条成功。"
  ```

  ```js
  t("index.recent_summary").replace("{total}", n).replace("{ok}", k)
  ```

## 2. 时间/日期格式化（各语言模式不同，必须成对定义）

| key | zh-CN | en | ja | es |
| --- | --- | --- | --- | --- |
| common.time.just_now | 刚刚 | just now | たった今 | ahora mismo |
| common.time.minutes_ago | {n} 分钟前 | {n} min ago | {n} 分前 | hace {n} min |
| common.time.hours_ago | {n} 小时前 | {n} h ago | {n} 時間前 | hace {n} h |
| common.time.days_ago | {n} 天前 | {n} d ago | {n} 日前 | hace {n} d |
| common.time.never | 从未使用 | Never used | 未使用 | Nunca usado |

热力图周标签/月份等同样各语言本地化（en: S M T W T F S；月份缩写 Jan…；ja/es 同理）。

## 3. 状态/枚举标签（全站统一一套，放 common）

`common.status.success/error/interrupted/info/warning`、`common.priority.high/normal/low`、`common.delivery.pending/sent/delivered/acked/failed` —— 各页面不许重复定义。

## 4. 演示数据（demo notifications / demo keys / demo devices）策略

**翻译**。演示数据是给用户看「这产品长什么样」的，日文用户看到中文演示数据等于没翻译。
键空间 `{page}.demo.*`，按现有结构逐条翻译（标题/正文/时间标签/设备名/标签值如「手机」「工作」→ 各语言）。
演示数据的「时间」字段（"2 分钟前"）复用 §2 的 time key + 占位符。

## 5. JS 交互文案（toast/confirm/prompt/alert）

全部走 t()：`{page}.toast.*`、`{page}.confirm.*`、`{page}.prompt.*`。
浏览器原生 confirm/prompt 里的 `{name}` 占位同样 replace。

## 6. docs.html 整页 {{t}} 化

- 所有正文/标题/表格/参数描述/提示框 → `{{t "docs.*"}}`
- **代码块内的示例 JSON 值**（"title": "部署完成"）→ 翻译为目标语言示例（用户照抄示例，看得懂更重要）；代码命令本身不译
- ASCII 时序图/规则图里的中文 → 翻译（保持对齐大致美观即可）
- key 量大，按页面分节命名：`docs.agent.*`、`docs.api.*`、`docs.auth.*`、`docs.notify.*`、`docs.webpush.*`、`docs.ws.*`

## 7. 翻译质量

- 技术术语保留英文：Passkey/Web Push/WebSocket/API Key/Service Worker/VAPID/WebAuthn/APNs/FCM
- ja 用です・ます体；es 用usted 体；en 简洁技术文档风
- 按钮/短标签各语言尽量短（es 最长，注意布局弹性）

## 8. 验收（每批必须）

1. `make sitegen && make build` 通过
2. 4 个 locale 文件 key 集合对齐（sitegen 单测已有校验，必须绿）
3. `go test ./...` 全绿
4. `make e2e-one S=i18n`、`make e2e-one S=frontend` 零回归
5. 自写临时脚本扫本批页面 en 版渲染 DOM，无中文残留（第三批后由 tester 固化为正式套件）
6. 注释英文

## 9. 批次划分（顺序执行，同一写线程）

- **批 1**：partials.js + index.html + message.html
- **批 2**：keys.html + receivers.html + security.html + login.html
- **批 3**：docs.html（{{t}} 化 + 4 语言全译）
