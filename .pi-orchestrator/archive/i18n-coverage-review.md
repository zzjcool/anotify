# 终审报告 · i18n 覆盖补全（批1/2/3 + i18n_coverage 套件）

**VERDICT: APPROVE（可合并）** — 3 处遗漏均为演示/边缘路径的残留裸串，不阻塞；建议合并后顺手修掉。

- **审查范围**：`git diff main...feat/i18n-coverage`（8f9def9 批1 / 6e2734d 批2 / e689f31 批3）+ 工作区未提交的 `scripts/e2e/suites/i18n_coverage.mjs`、`run_all.sh` 修改
- **审查方式**：只读审查 + 独立脚本核对（key 对齐/占位符/遮蔽/裸串扫描），未改代码
- **已验证事实采信**：全量 e2e 13/13 绿、go test 绿、timeAgo 遮蔽 bug 已修——抽查复核属实

## 逐项核对结论

| 终审要点 | 结论 |
| --- | --- |
| 1. key 对齐 | ✅ 4 语言各 545 key，missing=0 / extra=0（独立 python 解析核对） |
| 1. 共享 key 复用 | ✅ common.status/priority/time/ttl/delivery 被各页正确复用；同值多 key 均为合理场景（nav 标签 vs 页面标题、demo 数据复用示例） |
| 2. 遮蔽类 bug | ✅ timeAgo 已修为 `ts`；全量 grep 仅余 message.html:110 `fmtTime` 内 `const t = Date.parse`——该函数不调用 t()，当前无害（🟢 建议改名防隐患） |
| 3. 占位符一致性 | ✅ 44 个带占位 key 全部核对；多行 .replace 链逐一确认（keys.summary 的 {total}/{active}/{demo} 等），无错位 |
| 4. 翻译质量抽查 | ✅ ja/es 各抽 8+ key（docs 长文案 + demo 数据）：术语保留正确（Passkey/Web Push 不译）、ja です・ます体、es usted 体、语义准确；docs.agent.intro 等含 HTML 的译文标签结构完整 |
| 5. docs 架构变更 | ✅ 正文移入 content 块后：6 个 section 锚点 id 齐、TOC scroll-spy（IntersectionObserver）逻辑不动、copyBlock 用 closest() 不受影响、`innerHTML` 仅余注释；{{t}} 转义问题根除 |
| 6. 测试套件质量 | ✅ innerText 扫描（排除 script 源码）、原生名豁免（中文/日本語）合理；ja 页 ZH_ONLY_WORDS + JA_MARKERS 双向校验设计好。⚠️ 一个覆盖盲区见 🟡-2 |
| 7. 红线 | ✅ 无硬编码色值新增、无 innerHTML 注入面、注释英文、无范围蔓延 |

## 发现的问题

### 🟡 建议（不阻塞合并）

1. **批2 遗漏 3 处裸中文串**（演示/边缘路径，主流不可见）：
   - `web-src/pages/login.html:354` — `doLogin()` 的 WebAuthn 不支持分支直接传裸中文给 showStatus；同一文案在 `doSignup()`（:447-450）已正确走 `t("login.status.unsupported")`。不支持 WebAuthn 的浏览器 + en/ja/es 页面下会显示中文。
   - `web-src/pages/security.html:296` — demo 会话 s_3 `lastActive: "昨日 21:40"` 裸串（兄弟项 s_1/s_2 均用 t()/time key）。
   - `web-src/pages/security.html:300` — demo 会话 s_4 `device: "会议室 PC"` 裸串，且 yaml 缺对应 demo key（s4 device 无 `security.demo.*` 条目）。
   - 修法：login 处换 `t("login.status.unsupported", ...)`（key 已存在）；security 两处补 demo key + 走 t()。
2. **i18n_coverage 套件存在 demo 数据覆盖盲区**：各页 demo 数据由「API 失败回退」触发（无页面解析 `?demo=1`，该参数实为 no-op），套件带会话跑 security.html 时走真实 API 路径 → DEMO_SESSIONS/DEMO_PASSKEYS 永不渲染 → 上面 🟡-1 的两处 demo 裸串正是因此漏检。建议：套件增加「无后端/断 API」场景的 security 页扫描，或在页面支持显式 demo 参数。
3. **login.status.*_short 三个 key 是死键**（login_success_short / signup_success_short / need_username_short，4 语言均无调用点）——批2 引入的冗余，建议删除防漂移。

### 🟢 可选

1. `message.html:110` `fmtTime` 内 `const t = Date.parse(iso)` 与 i18n `t()` 同名不同域——当前无害（该函数不调 t()），但属 timeAgo 同款隐患，建议改名 `ts` 一劳永逸。
2. D1 断言在无 Key 可停用时软通过（`H.check(..., true, ...)` 形同虚设的分支）——demo 扫描已覆盖该文案，可接受，建议后续补一条强制路径。

## 需求契合度

覆盖「运行时 JS 文案 + docs 整页 i18n、4 语言、en/es 零中文残留」目标：达成（en/es × 7 页渲染零残留有自动化门禁）。3 处遗漏均在「不支持 WebAuthn 的浏览器」与「demo 会话数据」边缘路径，不影响主流程验收。
