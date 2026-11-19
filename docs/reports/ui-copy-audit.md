# 产品 UI 里的「开发过程叙述」：全量审计 + 清理 + 机械守卫

> snapshot-doc: 2026-11-19 本轮审计当次的实测定格（文中计数是当时的值，不是「现在是多少」的持续承诺；复算命令见 §7）

- 分支：`fix/ui-copy-audit`（基线 `dfadbab5` = 当时的 `origin/main`）
- 用户原话（契机）：「**搞笑吧？你没有审核出来吗？**」——节点页上挂着一整块卡片：

  > **命名空间与系统配置不在这页**
  > 命名空间 vars 是命名空间级配置（已迁到命名空间页）；系统保留期是系统级配置（已迁到管理后台）。节点页只保留「这个节点能被哪些命名空间使用」的授权列表。

  这段本身已由 PR #447（`9ec8afff`，分支 `fix/nodes-page-copy`）删掉。
  **但真正的问题不是这一段**：门禁完全抓不到这类文案——`check-i18n-keys` 只验「key 在 4 份 locale 里都在且集合一致」，
  e2e 只验页面 200 / 元素在。**「这条文案写得对不对」整个没有检查面**，
  所以只要 key 补全了，任何旁白都能一路绿到生产。这轮补的就是这个面。

---

## 0. 范围与方法

| 面 | 结论 |
| --- | --- |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | **唯一真相源**：4 份共 9316 条非空值（修复前） |
| `web-src/pages/*.html`（25 个页面） | **零散文**：剥掉注释 / `<script>` / `<style>` / `{{…}}` 后，渲染出的可见文字只剩符号、代码样例与 ASCII 图（198 行），全部散文都走 `{{t "…"}}` / `t("…")` |
| `/* */` / `<!-- -->` / `//` 里的开发注释 | **不算**、不动（它们本来就该写给维护者看） |

页面侧为什么零散文（可复算）：见 §7 的 `scan-pages2` 配方。顺带确认了第 5 类「悬空指代」在页面侧不存在，
只在 locale 值里有一处，且**有真实锚点**（见 §2 的「保留」条目）。

扫描口径：按 5 类特征（迁移叙述 / 对照内部实现 / 对开发者喊话 / 解释「为什么这么设计」/ 悬空指代）
写了宽口径正则先尽量捞全（宽口径共 51 处命中 / 22 个 key），**再逐条人工判定**，
而不是拿宽口径直接当门禁。这三步分开做很重要——直接拿宽口径当门禁会一次上线 30+ 条误报。

---

## 1. 清单（文件:行 → 原文 → 分类 → 判定）

行号是**修复前**（`dfadbab5`）的行号；4 份 locale 同键同行位（zh-CN 与其余三份差 0~2 行，差异已核对）。

### 1.1 删（8 个 key × 4 语言 = 32 行）

| # | key | zh-CN 行 | 分类 | 原文（zh-CN） | 判定理由 |
| --- | --- | --- | --- | --- | --- |
| 1 | `nodes.v4.moved_title` | 1813 | A 迁移叙述 | 命名空间与系统配置不在这页 | 用户投诉原文。讲「不在这」，不讲这页能做什么 |
| 2 | `nodes.v4.moved_note` | 1814 | A 迁移叙述 | 命名空间 vars 是命名空间级配置（**已迁到**命名空间页）；…本页只保留… | 同上；「已迁到」是开发过程的时间线 |
| 3 | `nodes.v4.moved_ns` | 1815 | 依附 #1 | 去命名空间页 | 只服务那块卡片的 CTA，卡片删了就没有宿主 |
| 4 | `nodes.v4.moved_system` | 1816 | 依附 #1 | 去管理后台 | 同上 |
| 5 | `docs.endpoints.intro` | 841 | C/D 构建管线 + 历史对照 | 这份清单在**构建时**从 api/openapi.yaml 生成…**已弃用的旧「通知终端」端点**不在此列 | 讲构建管线与旧端点，用户拿不到任何可执行信息；且全仓无人引用 |
| 6 | `docs.endpoints.empty_title` | 843 | C 对开发者喊话 | 端点清单未生成 | 「未生成」是构建态；用户看到只会以为产品坏了 |
| 7 | `docs.endpoints.empty_desc` | 842 | C 对开发者喊话 | 构建时没有加载到 api/openapi.yaml。**重新运行 make sitegen** 即可生成这一节 | 直接叫用户跑构建命令；用户没有仓库、没有 make |
| 8 | `pb.cap.declared.legacy` | 2262 | B 历史形态 | 旧形态声明 | 「旧形态」是历史标签；且全仓无人引用（`pb.v4.declared` 才是活的） |

> #5–#8 同时是**死词条**（`check-i18n-keys` 的 unreferenced 告警里就有它们）。
> 这一轮只清「是死词条**且**是开发叙述」的那几条；剩下 766 条死词条属于另一个批次，不在本次范围。

### 1.2 改写（13 个 key × 4 语言）

| # | key | zh-CN 行 | 分类 | 判定理由 |
| --- | --- | --- | --- | --- |
| 9 | `docs.toc.design` | 832 | 4 解释设计 | 目录标签「设计取舍说明」= 对内的设计决策，产品目录不该有这个词 |
| 10 | `docs.design.title` | 834 | 4 / C | 「设计取舍说明：**为什么界面这样显示**」——整节的存在理由就是解释实现 |
| 11 | `docs.design.lede` | 835 | **C 对开发者喊话（最露骨）** | 「下面这些是**写给维护者与排障者**的说明。**控制台首屏只保留**…」 |
| 12 | `docs.design.nodes` | 838 | 4 / B | 「**不是 Playbook 的整包 0 写规则**」（与另一处内部规则对照）+「都来自**真实 GET**，缺字段时…不**冒充**」（讲实现） |
| 13 | `nodes.v4.batch_hint` | 1810 | 4 / B | 「**不是 Playbook 的整包 0 写规则**」——同上，用户不知道这条规则是什么 |
| 14 | `docs.v4.five_specs` | 1022 | B 内部实现 | 「五份 **v4** spec；**未接入时真实空态，不展示旧 spec 端点**」——版本代号 + 未接入状态 + 旧 spec 对照 |
| 15 | `docs.v4.missing` | 1023 | B 内部实现 | 空态标题写成「尚未加载五份 **v4** spec」——用户视角应是「暂不可用」 |
| 16 | `docs.v4.node_note` | 1026 | B 历史对照 | 尾句「**没有旧 agent 命令、端口路由或 priority**」——讲 v3 退役了什么 |
| 17 | `sources.v4.edges` | 1913 | B 内部版本 | 「只读 **v4** 目标能力投影」 |
| 18 | `sources.v4.graph_hint` | 1915 | B 历史对照 | 「图只展示 **v4** 引用，**不包含端口、优先级或独立接线**」——拿退役概念当参照系 |
| 19 | `source_detail.updated_hint` | 2588 | B 内部版本 | 「**v4** 没有独立的更新时间字段…」 |
| 20 | `docs.webpush.off_note` | 974 | C/D 实现 + 版本 | 「**服务端未启用 VAPID**，**本版**不提供设备订阅页」——运维配置 + 发版口径 |
| 21 | `uxaudit.res.nodes.note_summary` | 2442 | **C 对开发者喊话** | 产品页上一个 `<summary>` 直接写「**开发者说明**：槽位判定与批量写规则」 |

### 1.3 保留（判定为正常文案，附理由）

宽口径捞出来的其余命中，逐条判定为误报，**不改**：

| 位置 | 命中词 | 原文 | 保留理由 |
| --- | --- | --- | --- |
| `login.recovery.code_placeholder`（四语言） | `XXX` | `XXXX-XXXX-XXXX-XXXX` | 恢复码**格式提示**，不是 TODO |
| `cliauth.entry.placeholder`（四语言） | `XXX` | `XXXX-XXXX` | 同上 |
| `docs.webpush.step3`（四语言） | `#123` | `pushManager.subscribe(&#123; …)` | HTML 实体 `&#123;`（`{`），`#` 前面是 `&` |
| `keys.security.point4`（en） | `commit ` | Never **commit** Keys to version control | 安全建议里的动词 |
| `keys.desc_tip`（zh） | 见下方 | …（**见下方「Key 的安全实践」**） | **锚点真实存在**（`security.practices.title` = 「Key 的安全实践」，同页渲染）；不是悬空指代 |
| `security.*.toast.removed`（zh） | 已移 | 密码**已移除** | 「已移除」是动作完成，不是「已迁」 |
| `admin.system.group_danger_note`（zh） | 改成 | 把值**改成** 0 等于永久保留 | 危险操作警示，正常文案 |
| `sources.fire_payload_hint` / `sources.form.bad_placeholder` / `sources.form.pv_broken` | 占位 | 源参数里的**占位符**从这里取值 / 占位符没闭合 | 「占位符」是模板变量的正确术语 |
| `pb.cap.impl.noop` / `prune.no_impl` | 占位 | `noop 占位` / 目标是**占位实现（noop）** | `noop` 是 playbook 里真实存在的实现种类，不是「还没实现」 |
| `events.page.older` / `caps.nodes.stale` | 旧 | **更旧** → / **旧** digest | 分页方向 / 陈旧态标签，都是「旧」的正常用法 |
| `docs.ws.reconnect.desc`（ja） | 移行 | その後リアルタイムストリームに**移行**します | 数据流状态迁移，不是「配置搬家」 |
| `receivers.pair.no_backend` 等 5 处（四语言） | make dev | （本地可运行 **make dev**） | 本仓自托管排障指引的正常文案 |
| `namespace.v4.yaml_hint`（en） | make it | the page will not **make it** public | 英文动词 |

### 1.4 页面侧

只有一处 DOM 变化：`web-src/pages/nodes.html` 删掉那块卡片（含它上方的 `IA：#436` HTML 注释）。
其余 24 个页面**无需改动**——它们没有散文，也没有开发叙述。

---

## 2. before → after（逐条，zh-CN 为准；四语言同步）

删掉的 8 个 key 无 after，见 §1.1。下面 13 条改写：

**9. `docs.toc.design`**
- before：`设计取舍说明`
- after：`行为与边界`

**10. `docs.design.title`**
- before：`设计取舍说明：为什么界面这样显示`
- after：`行为与边界：这些地方容易误解`

**11. `docs.design.lede`**
- before：`下面这些是写给维护者与排障者的说明。控制台首屏只保留「要你做决定」的信息，原理与边界集中在这里。`
- after：`下面这几条是产品行为里最容易误读的地方，出问题时按它们对账。`

**12. `docs.design.nodes`**
- before：`节点配置可以逐台编辑；批量配置 API 逐节点原子，允许部分成功，不是 Playbook 的整包 0 写规则。保留期单位为秒，0 表示永久；默认值为日志/产物 604800 秒、事件与终态任务 2592000 秒，活动任务与引用保护仍然生效，用户的 workDir 不会被清理。首屏的配置版本（configRevision）与已应用版本（appliedRevision）都来自真实 GET，缺字段时界面显示「未知」而不以默认值冒充。`
- after：`节点配置可以逐台编辑；批量配置 API 逐节点原子，允许部分成功。保留期单位为秒，0 表示永久；默认值为日志/产物 604800 秒、事件与终态任务 2592000 秒，活动任务与引用保护仍然生效，用户的 workDir 不会被清理。配置版本与已应用版本显示的是服务端当前值，读不到时显示「未知」，不会填默认值。`
- 保留了什么：「逐节点原子、允许部分成功」是真实的 API 行为；保留期默认值与「workDir 不会被清理」是用户会用来对账的事实；「读不到显示未知，不填默认值」是用户能观察到的承诺。去掉的是与另一处内部规则的对照、以及「真实 GET / 冒充」这种实现叙述。

**13. `nodes.v4.batch_hint`**
- before：`配置可逐台编辑；批量 API 逐节点原子，允许部分成功，不是 Playbook 的整包 0 写规则。`
- after：`配置可逐台编辑；批量 API 逐节点原子，允许部分成功。`

**14. `docs.v4.five_specs`**
- before：`端点来自 resource / events / agent / ingress / auth 五份 v4 spec；未接入时真实空态，不展示旧 spec 端点。`
- after：`端点来自 resource / events / agent / ingress / auth 五份 spec。`
- 保留了什么：五份 spec 的**名字**（用户要按名字找 endpoint）。去掉版本代号与「未接入 / 旧 spec」的过程叙述。

**15. `docs.v4.missing`**
- before：`尚未加载五份 v4 spec`
- after：`端点清单暂不可用`
- ⚠️ 这条**只能改写、不能删**：`internal/sitegen/openapi_test.go:1222` 断言空态页不得泄漏裸 key `docs.v4.missing`。删掉 key 会让空态渲染出 key 名，测试立刻红。

**16. `docs.v4.node_note`**
- before：`先登记 Node，再签发长期身份令牌。本机申报与授权取交集；install 不自动注册。没有旧 agent 命令、端口路由或 priority。`
- after：`先登记 Node，再签发长期身份令牌。本机申报与授权取交集；install 不自动注册。`

**17. `sources.v4.edges`**
- before：`只读 v4 目标能力投影`
- after：`只读的目标能力引用`

**18. `sources.v4.graph_hint`**
- before：`图只展示 v4 引用，不包含端口、优先级或独立接线；不是执行状态。`
- after：`图只展示能力引用，不代表执行状态。`

**19. `source_detail.updated_hint`**
- before：`v4 没有独立的更新时间字段：每次写入以 resourceVersion 递增体现。`
- after：`没有独立的更新时间字段：每次写入以 resourceVersion 递增体现。`
- 只去掉版本代号，信息量不动。

**20. `docs.webpush.off_note`**
- before：`手机推送（Web Push）暂未开放：服务端未启用 VAPID，本版不提供设备订阅页。事件仍会正常产生，可在「事件流」页查看。`
- after：`手机推送（Web Push）暂未开放：开放后这里会出现订阅步骤。事件仍会正常产生，可在「事件流」页查看。`
- 去掉「服务端未启用 VAPID」（运维配置）与「本版」（发版口径），换成用户会看到的变化。

**21. `uxaudit.res.nodes.note_summary`**
- before：`开发者说明：槽位判定与批量写规则`
- after：`槽位判定与批量写规则`
- 这是产品页 `<details>` 的 `<summary>`；内容（槽位判定 + 批量写规则）是真实的产品规则，只删掉「开发者说明：」这个对谁说话的抬头。

英文 / 日文 / 西班牙文的新值见 `web-src/locales/*.yaml` 的 diff，语义与上面逐条对齐（不做直译，按各语言习惯重写）。

---

## 3. 守卫设计：`scripts/check-ui-copy.mjs`

### 3.1 判据

扫 `web-src/locales/*.yaml` 的**值**（`parseLocale` 给的行号；注释行与 key 名天然跳过），
命中高危词表即失败，逐条打 `文件:行 → «命中词» → 原文`，并附 `[规则码] 为什么`。

- **20 条规则**，分 2 组：语言无关 5 条（`issue-ref` `internal-version` `docs-path` `build-tool` `dev-todo`），
  按语言分组的迁移/历史/对开发者喊话/构建管线 15 条（zh / en / ja / es）。
- **豁免**：值所在行或**上一行**的注释里写 `# ui-copy-ok: <理由>`。
  理由必填——**空理由仍然失败**（`# ui-copy-ok:` 后面什么都没有 → 报「有标记但没写理由」）。
  标记必须落在**注释里**：写在值里的同形文字不算（自测 `exempt-5` 钉住这点）。
- **反假绿**：打印 `文件数 / 值数 / 规则数`，并对文件数（≥4，且四份语言都在）与值数（≥9000）设下限。
  解析出空值集一律失败——「绿」与「没检查」不能同形（本仓反复吃这个亏）。
- **fail-loud**：locale 解析复用 `check-i18n-keys.mjs` 的 `parseLocale`，不认识的 YAML 语法抛
  `ParseError` → 退出码 2，绝不静默跳过。
- **`--self-test`**：红样例 **17** / 绿样例 **12** / 豁免 **5**（要求红 ≥6、绿 ≥4）。
  红样例断言的是**哪条规则**命中（防「碰巧被别的规则抓到」的假红），
  并额外做一次**反向证明**：把命中词从红样例删掉必须转绿（防「这条规则永远红」的假实现）。

### 3.2 词表「窄而准」是怎么落地的（含被故意排除的高频词）

先按 5 类特征做宽口径扫描（51 处），再逐条判定，只留**零误报**的词。定稿词表在修完的 locale 上跑 = **命中 0 条**。

被**故意排除**的词（都会误报，实测证据在 §1.3）：

| 候选词 | 误报 | 处置 |
| --- | --- | --- |
| 「make + 目标名」 | `make dev`（自托管排障，5 处）、`make test`（容器跑能力示例）、英文的 "make it" / "make sure" | **整条剔除**，只查 `sitegen`（唯一真正的构建工具泄漏） |
| `\b[0-9a-f]{7,40}\b`（commit sha 形状） | 命中 `2592000` 这种秒数 | 剔除 |
| `移行`（ja） | `…リアルタイムストリームに移行します`（数据流状态迁移） | 收窄成 `へ移動｜移動しました｜このページにはありません` |
| `旧 `（zh/ja 裸词） | `更旧 →`（分页）、`旧 digest`（真实陈旧态标签） | 收窄成 `旧 spec｜旧版本｜旧「｜旧形态｜旧 agent｜旧接口｜旧端点｜已弃用` |
| `XXX` | `XXXX-XXXX`（恢复码格式提示） | 剔除（TODO/FIXME 已够） |
| `legacy`（zh 侧） | — | 只在 en 词表保留；中文侧用具体词 |
| `弃用`（裸词） | 可能命中正常弃用说明 | 收窄成 `已弃用` |

### 3.3 接线

- 新目标 `make check-ui-copy`（先 `--self-test` 再实跑，对齐 `check-i18n-keys` 的形状）
- 挂进 `fe` 的前置，**紧跟 `check-i18n-keys`**（`make ci` → `make fe`，所以 `ci` 覆盖到它）
- 同时进 `check-guard-selftest`（「守卫自己静默不查了」的那一档）
- `.PHONY` 已加

---

## 4. 变异验证（守卫确实会红）

### 4.1 把 #447 删掉的原文加回 locale → 红

用 `git show 9ec8afff` 取回被删的**原文**（不是重写版），按原位置插回 4 份 locale 的 `node_subtitle` 之后，跑守卫：

```
✗ 文案违规：7 条（用户可见文案里混进了开发过程叙述）
  检查量：4 文件 / 9300 条值 / 20 条规则（下限 文件≥4、值≥9000）

  web-src/locales/en.yaml
    web-src/locales/en.yaml:1808 → «not on this page» → Namespace and system configuration are not on this page
      [en-migration] 迁移叙述（英）   (key: nodes.v4.moved_title)
    web-src/locales/en.yaml:1809 → «moved to» → Namespace vars are namespace-level configuration (moved to the namespace page); retention is system-level configuration (moved to the admin console). This page …
      [en-migration] 迁移叙述（英）   (key: nodes.v4.moved_note)

  web-src/locales/es.yaml
    web-src/locales/es.yaml:1809 → «movidas a» → Las vars de un espacio de nombres son configuración de ese espacio (movidas a su página); la retención es configuración del sistema (movida al panel de administrac…

  web-src/locales/ja.yaml
    web-src/locales/ja.yaml:1808 → «ページにありません» → ネームスペース設定とシステム設定はこのページにありません
      [ja-migration] 迁移叙述（日）   (key: nodes.v4.moved_title)
    web-src/locales/ja.yaml:1809 → «へ移動» → ネームスペースの vars はネームスペース単位の設定（ネームスペースページへ移動）、…

  web-src/locales/zh-CN.yaml
    web-src/locales/zh-CN.yaml:1810 → «不在这页» → 命名空间与系统配置不在这页
      [zh-migration] 迁移叙述：讲东西搬到哪去了，而不是这块能做什么   (key: nodes.v4.moved_title)
    web-src/locales/zh-CN.yaml:1811 → «已迁» → 命名空间 vars 是命名空间级配置（已迁到命名空间页）；系统保留期是系统级配置（已迁到管理后台）。…
      [zh-migration] 迁移叙述：讲东西搬到哪去了，而不是这块能做什么   (key: nodes.v4.moved_note)
EXIT=1
```

四语言**各自**命中（不是靠中文兜住），撤掉后回到 `EXIT=0`。

### 4.2 修复前基线 = 57 条

同一守卫跑在**未修**的 `origin/main` locale 上（`git checkout` 时意外复原，正好当基线取证）：

```
✗ 文案违规：57 条（用户可见文案里混进了开发过程叙述）
  检查量：4 文件 / 9316 条值 / 20 条规则（下限 文件≥4、值≥9000）
```

恢复修复后：

```
✅ 文案守卫通过：4 文件 / 9284 条值 / 20 条规则（下限 文件≥4、值≥9000）
```

### 4.3 自测（红/绿两侧都证明）

```
$ node scripts/check-ui-copy.mjs --self-test
check-ui-copy --self-test
  规则 20 条；红样例 17 / 绿样例 12 / 豁免 5
  ✅ 自测通过：红样例全红、绿样例全绿、豁免正反都对
```

---

## 5. 门禁数字

| 项 | 值 |
| --- | --- |
| `node scripts/check-ui-copy.mjs` | ✅ 4 文件 / 9284 条值 / 20 条规则（下限 4 / 9000），命中 0 |
| `node scripts/check-ui-copy.mjs --self-test` | ✅ 17 红 / 12 绿 / 5 豁免全对 |
| `node scripts/check-i18n-keys.mjs` | ✅ 4 份 locale 键集合一致；2324 个 key / 2189 个引用点全部命中词条（766 个未被引用仅告警） |
| `node scripts/check-annotations.mjs` | ✅ `unbacked 300 / 基线 300`、`unmarked 385 / 基线 385` — **基线上调 0**（本轮的注释按规程收窄措辞，没有动 `CLAIM_UNBACKED_BASELINE` / `COUNT_UNMARKED_BASELINE`） |
| `make fe` | ✅ class 校验 877 类 / check-css / check-i18n-keys / **check-ui-copy** / contrast / sitegen（4 语言 × 23 页）+ 指纹 31 资源 / 92 HTML |
| `make ci` | ✅ 全绿（见 §6） |

`check-annotations` 的两处易踩点，本轮都按规程处理而不是放宽：
1. 新增的 `.mjs` 注释里出现「门禁 / 守卫 / 会红」会被规则 C 计入未锚定声明，出现 `\d+ 条/个/行/处` 会被规则 D 计入未标记计数——
   所以新脚本的注释**收窄措辞**，把数字留在代码常量（`FLOORS`）与本文档（带 `snapshot-doc:` 定格标记）里。
2. 注释/文档里的行内反引号会被当成**引用**校验：把英文的 make it 写进反引号会被判成「Makefile 里没有这个目标」，
   所以这类英文动词短语一律不加反引号；`scripts/check-ui-copy.mjs` 与 `make check-ui-copy` 本身都存在，正常解析。

---

## 6. `make ci` 实跑

在**最终树**上整跑一次（不是拆开跑各段），退出码 0：

```
✅ class 校验通过：877 个类（HTML class 属性 628 + JS class 串新增 249）全部落在设计系统 / Tailwind 工具类内
✅ i18n 键守卫：4 份 locale 键集合一致，2189 个引用点全部有词条（766 个未被引用的 key 仅告警）
✅ 文案守卫通过：4 文件 / 9284 条值 / 20 条规则（下限 文件≥4、值≥9000）
✅ sitegen done: 4 languages × 23 pages → web
  unbacked 300 / 基线 300（约束性注释行共 300）
  unmarked 385 / 基线 385（计数型注释行共 386）
✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。
✅ harness 自测通过
CI_EXIT=0
```

工作区是独立 worktree，磁盘上**没有** `.pi-orchestrator/`，
避开了「本地绿 CI 红」那种不一致。

---

## 7. 复算配方（自己验证这份报告）

```bash
# 1. 守卫本身
node scripts/check-ui-copy.mjs --self-test && node scripts/check-ui-copy.mjs

# 2. 变异：把 #447 的原文插回去，必须红；再撤掉
git show 9ec8afff -- web-src/locales/zh-CN.yaml | sed -n 's/^-\s\{4\}/    /p'   # 取出被删的 4 行
#   手工插回 nodes.v4.node_subtitle 之后 → node scripts/check-ui-copy.mjs 应 EXIT=1 → git checkout --

# 3. 页面侧「零散文」复核：剥掉注释/script/style/{{…}} 与标签后，只应剩符号与代码样例
node -e '
const fs=require("fs"),path=require("path");
const R="web-src/pages", blanks=m=>m.replace(/[^\n]/g," ");
let n=0;
for(const f of fs.readdirSync(R).filter(f=>f.endsWith(".html"))){
  const raw=fs.readFileSync(path.join(R,f),"utf8");
  const vis=raw.replace(/<!--[\s\S]*?-->/g,blanks).replace(/<script\b[\s\S]*?<\/script>/gi,blanks)
    .replace(/<style\b[\s\S]*?<\/style>/gi,blanks).replace(/\{\{[\s\S]*?\}\}/g,blanks).replace(/<[^>]+>/g,blanks);
  vis.split("\n").forEach(l=>{ if(l.trim()) n++; });
}
console.log("可见文字行（应全是符号/代码样例）:",n);'
```

---

## 8. 遗留风险

1. **词表是按「已知事故形状」长出来的，不是完备的语义分类器。** 它拦 5 类特征里的**具体措辞**，
   拦不住「用全新的措辞讲同样的事」。这条守卫的性质是**棘轮**：每来一次新形状，就往词表加一条窄词，
   不要为了「更全」把词放宽（放宽立刻产生误报，然后就会有人加豁免，最后等于没有守卫）。
2. **`v4` 规则治标。** 只要 `v4` 仍是内部版本代号且是唯一在跑的版本，`\bv4\b` 在 locale 值里应恒为 0。
   将来若真要面向用户区分版本（例如升级迁移期），应当先引入一个**用户可见的产品版本名**，而不是解开这条规则。
3. **`docs.design.*` 这一节的性质仍有残余张力。** 它是 docs 页（本身就是给集成者看的），
   所以 `sources` / `playbooks` 两段保留了 `spec.targets`、`POST …/install` 这类 API 叙述——那对集成者是有效信息。
   本轮只清掉了「写给维护者与排障者」「为什么界面这样显示」「不是 Playbook 的整包 0 写规则」「真实 GET / 冒充」这些**过程叙述**。
   若后续认为整节应当移出产品站，那是信息架构的决定，不是文案 bug。
4. **766 条未被引用的死词条**没清（只清了 4 条「死 + 是开发叙述」的）。死词条清扫是独立批次。
5. **PR #447 与本分支改的是同一块**（`nodes.v4.moved_*` + `nodes.html`）。两边都是纯删除，
   git 合并时是「双方都删同一段」→ 不冲突；但若 #447 先合，本分支重跑 `make fe` 即可，locale 内容一致。
