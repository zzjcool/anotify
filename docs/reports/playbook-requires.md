> **历史快照**：本文件是 #534 交付时的定格记录，不随代码演进；行为以代码、测试与 `api/resource.yaml` 为准。

<!-- snapshot-doc: 2026-11-22 #534 交付时的定格数字（make ci / e2e / 变异实测），复算命令见 §4 -->

# 预案 `requires` 依赖锁（#534 · #512 的 D 阶段）交付报告

> 分支 `pi-subagent/host-workspace/anotify-534-requires-b43ee046` · 基线 `origin/main` `c4d01644`
> （含 #533 `kind: poll` / 迁移块 6）· 设计真源 issue #512（D 阶段）· 需求真源 issue #534
> 冻结契约：`.pi-legion/contract.md`（团队工作区，不入库）

一句话：**`requires` 是「装之前先看别人有没有」的声明——安装期把它解析成具体版本（缺一条就整包 0 写），
装完之后它就变成一把锁（被依赖的能力删不掉，卸载那个 release 才放行）。**

---

## 1. 两半各自的落点

### 1.1 安装期校验（缺依赖 → 409 + 整包 0 写）

| 层 | 文件:位置 | 做了什么 |
| --- | --- | --- |
| 包解析 | `internal/playbook/parse.go:167` | `spec` 白名单加 `requires`；随后 `pkg.Requires = parseRequires(spec, EntryFile)` |
| 形状校验 | `internal/playbook/requires.go:parseRequires` | 未知键 / 非数组 / 条目非对象 / 名字非法 / `minVersion` 非 ≥1 整数（含显式 null）/ 重复 / 超 32 条 → 400 `invalid_playbook`，**全部问题一次收进 `details.causes[]`**，field 精确到 `spec.requires[i].<键>` |
| 解析 | `internal/playbook/requires.go:resolveRequires` | 先自引用（400），再逐条查命名空间里该能力的**当前可见最高版本**（`Existing.CapabilityMaxVersion` → `SELECT MAX(version) … deleted_at IS NULL`，与 `GET …/capabilities/{name}` 同一可见性判据）；不存在 → message 带 `have: none`；版本不够 → `need: N, have: M`；任一未满足 → 409 `dependency_missing` |
| 调用点 | `internal/playbook/plan.go:219`（`PlanForRelease` 内，render 之后、12 格矩阵之前） | 依赖类错误优先于内容冲突；**dryRun 与真装同一条路径**（服务端单测 `TestPlaybookInstallRequiresDryRunUsesSamePath` 钉住「两份 409 的 field/message 逐字相同」） |
| 0 写 | `internal/store/playbook.go:436-452` | plan 错误在任何 INSERT 之前返回；安装本来就单事务 + `defer Rollback` |
| 落库 | `internal/store/playbook_tx.go:insertPlaybookInstallOn` | 回执的 `dependencies` 逐条写进 `playbook_install_requires`（同一次事务；dryRun 随回滚） |
| 回执 | `internal/playbook/playbook.go`（`Receipt.Dependencies`） | 安装成功时记 `dependencies: [{capability, version}]`（version = 解析到的可见最高版本，不是 `minVersion`）；无依赖恒 `[]` |
| wire | `internal/server/resource_playbook.go:writePlaybookAPIError` | `dependency_missing` → **409**（`details.causes[]`）；带 causes 的 400 也终于带上 `details`；无 causes 的错误响应逐字不变 |

### 1.2 依赖保护（被依赖的能力删不掉）

| 层 | 文件:位置 | 做了什么 |
| --- | --- | --- |
| 判定 | `internal/store/capability_lock.go:capabilityConsumersOn` | `playbook_install_requires ⨝ playbook_installs ⨝ playbooks`，`uninstalled_at = 0`，按 `(slug, release)` 去重 |
| 拦截 | `internal/store/resource_cas.go:132-138`（`DeleteCapability`，CAS 校验后、第一条 UPDATE 前） | 有消费者 → `*CapabilityLockedError`，整事务回滚（0 写） |
| wire | `internal/server/api_status.go:capabilityLockDetails` | 409 `dependency_locked`，`details.causes[]` 一条一个消费者（message 里点 slug + release，field `name`） |
| 索引 | `internal/store/schema.sql` 迁移块 7 `playbook_requires` | `playbook_install_requires(namespace_id, install_id, capability, version)` + `(namespace_id, capability)` 索引 + 两条 `ON DELETE CASCADE`；`headVersion` 6 → 7 |

### 1.3 五件套

| 件 | 落点 |
| --- | --- |
| `api/resource.yaml` | `PlaybookRequirement`（`capability`/`minVersion`，`minVersion` minimum 1）与 `PlaybookDependency`（`capability`/`version`）两个 component；`PlaybookReceipt.dependencies`（不进 `required`：旧回执没有）；`installPlaybook` 的 409 与 `deleteCapability` 的锁语义（description）；`Status409` / `StatusDetails` / `StatusCause` 的 prose 补两条 reason 与 field 形状 |
| parity | `internal/server/openapi_parity_test.go:parityPairs()` 加 `playbook.Requirement` / `playbook.Dependency` 两对（**没有**动 `unpairedSchemas` / `wideSchemas` / 任何下限）；实测 `total=253 paired=213 unpaired=16`（改前 251/211/16） |
| DTO | 手写 DTO（本仓无代码生成）：`playbook.Requirement` / `playbook.Dependency` / `playbook.Cause` / `Receipt.Dependencies` / `store.DependencyConsumer` / `store.CapabilityLockedError` |
| e2e | `scripts/e2e/suites/playbook_release.mjs` 新增第 11 节 `partRequires`（60 条断言）：`285 通过 / 0 失败`（改前 225） |
| 单测 | `internal/playbook/requires_test.go`（写入期 13 条 + 解析 6 条）、`plan_test.go`（`TestPlanRequires` + 对照组）、`receipt_test.go`、`internal/store/capability_lock_test.go`（5 例）、`internal/server/playbook_requires_test.go`（7 例） |

---

## 2. 十条验收（命令 + 实测）

| # | 判据 | 实测证据 |
| --- | --- | --- |
| 1 | 缺依赖 → 409，四张表指纹逐字节相同 | e2e §11-①：`requires: [{capability: nope}]` → 409 `dependency_missing`、`causes[0].field = spec.requires[0].capability`、message 含 `have: none`；`before/after` 指纹逐字节相同。指纹刻意做成 **6 张表**：`capability_versions` / `sources` / `edges` / `playbook_installs` / **`playbook_versions`** / **`playbook_install_requires`**（比 issue 要求的四张更严） |
| 2 | 版本不够 → 409，message 同含 `need: 2` 与 `have: 1` | 服务端单测 `TestPlaybookInstallRequiresVersionTooLow`：`inventory` v1 + 包要 `minVersion: 2` → 409，message 同含 `need: 2` 与 `have: 1`（正是 issue 的字面值）；e2e §11-② 用 v2 要 3 → 同含 `need: 3` / `have: 2`，且指纹 0 写 |
| 3 | 满足即装，回执写具体版本 | e2e §11-③：`minVersion: 2` + v2 在 → 200，回执 `dependencies = [{"capability":"inventory","version":2}]`（**断言具体版本号**，不是只断言存在）；单测 `TestPlanRequires` / `TestReceiptDependenciesFilled` 同判据 |
| 4 | 锁：删被依赖能力 → 409；卸载 release 后放行 | e2e §11-④：`DELETE …/capabilities/inventory?resourceVersion=…` → 409 `dependency_locked`（causes 点名 `pb-req-ok` / release `q`），再 GET 证明 0 写（rv 未推进）；`DELETE …/releases/q` 后**同一令牌**再删 → **204**（issue 写 200，DELETE 无 body 在本仓是 204） |
| 5 | 写入期五种 → 400 且 field 指到 `spec.requires[i].…` | e2e §11-⑤：未知键 `spec.requires[0].bogus`、`minVersion: 0` → `[0].minVersion`、非法名 → `[0].capability`、重复 → **[1].capability**（建包端点）；自引用：建包 201、install → 400 `invalid_playbook` + `[0].capability`。单测把每条 field 逐字钉死，另补 `minVersion: null` / 负数 / 非整数 / `capability` 非字符串 |
| 6 | 不误伤：`requires` 省略时行为一致（对照组） | e2e §11-⑥：**不写** `requires` 键的包 → 200、`dependencies == []`、`/installs` 仍折叠出 1 组、uninstall 200；显式 `requires: []` 的包同判据。单测 `TestPlanWithoutRequiresIsUnchanged`（`pkg.Requires == nil`、`len(plan.Dependencies) == 0`、`Created/Skipped/Upgraded/Unapplied/External/Pending/Notes` 与矩阵行 1 逐项一致）+ `TestPlaybookInstallWithoutRequiresKeepsResponseShape`（无 causes 的 400 不带 `details`，与今天一致）。**注意强度**：这是字段级/状态码级对照，不是响应体金样逐字比对（见 §5-风险 3） |
| 7 | 两条变异必红后复绿 | §3 |
| 8 | `make ci` exit 0；`make e2e-affected` 绿；`known-env-failures.json` 保持空 | §4 |
| 9 | 五件套 | §1.3 |
| 10 | UI：缺依赖显示缺什么；删除被拦显示被谁依赖 | `web-src/pages/playbook.html`（`dependency_missing` → 复用既有 `pb-warning` 块逐条列 `field · message`，message 原样，`need:`/`have:` 不被改写）；`web-src/pages/capability.html`（`dependency_locked` → `#del-msg` 拼 `details.causes[].message`，**不**进 CAS 重载分支）；4 份 locale 各 +2 键。机械判据：`make check-classes`（无自造类）、`make check-i18n-keys`（4 份键集合一致，新键都被引用）、`frontend` chrome 套件（页面无 JS 错误）。**缺口（诚实说明）**：没有针对这两条 409 的**浏览器级分支断言**（见 §5-风险 2） |

---

## 3. 两条变异（先红后绿）

变异方式：工作区改代码 → `./scripts/e2e/run_all.sh playbook_release` → `git checkout` 复原 → 再跑一次。

| 变异 | 改法 | 结果 |
| --- | --- | --- |
| ① 去掉安装期校验 | `plan.go` 删掉 `resolveRequires` 调用（`deps := []Dependency(nil)`） | **265 通过 / 20 失败**；首条红 `requires 指向不存在的能力 → 409 dependency_missing — 期望 409 实际 200`；自引用也变 200 |
| ② 回执记录但删除时不查引用 | `resource_cas.go` 删掉 `capabilityConsumersOn` + `CapabilityLockedError` 分支 | **279 通过 / 6 失败**；首条红 `被已生效安装依赖的能力 → 409 dependency_locked — 期望 409 实际 204` |
| 复绿 | 两次 `git checkout` 复原 | **285 通过 / 0 失败**（5.0s） |

另有一条**审查驱动的变异**（杀掉「漏写能力过滤」的弱实现）：把 `capability_lock.go` 的 `AND r.capability = ?` 删掉 →
`TestDeleteCapabilityLockedByRequires` 立刻红（`锁只认被 requires 点名的能力，没被依赖的 other 照旧可删`），复原后绿。

---

## 4. 门禁数字（最终提交实测）

### 4.1 `make ci` → **exit 0**

```
{{CI}}
```

### 4.2 `make e2e-affected` → **绿**

`api/**` 与 `internal/store/schema.sql` 落在 `FULL_SURFACES`，选择器按设计升为**全量**（30/30 套）：

```
{{E2E}}
```

- `scripts/e2e/known-env-failures.json`：`entries: []`，本分支 diff 为空（`git diff origin/main...HEAD -- scripts/e2e/known-env-failures.json` 无输出）
- `scripts/check-comment-density.mjs`：**一字未改**（diff 为空），目录注释行逐项等于基线（总量 `43330 = 43330`）

---

## 5. 设计决策、审查跟进与风险

### 5.1 决策（判「缺失」前先看这里）

| 编号 | 决策 | 理由 |
| --- | --- | --- |
| D1 | 409 只在 **install**（含 `dryRun`）产生；`POST …/playbooks`（建包）只跑写入期 400 | 建包只是把声明落库：此刻没有安装事实、没有回执可审计，也没有「解析到哪个版本」可记。issue 的「三条路径都跑」落成「三条路径都跑**写入期**校验，安装两条再跑解析」 |
| D2 | `requires[].capability` 是**字面量**，不参与 `{{values.*}}` 渲染 | 写入期就要判名字规则（issue 语义 4），模板名无法在写入期判 |
| D3 | 自引用在 plan 里判（渲染后才知道本包装的能力叫什么），错误是 **400** `invalid_playbook` | 这是包的声明错误，不是与库状态的冲突 |
| D5 | `details.causes[]` 沿用三键 `{reason, message, field}`，`need`/`have` 写进 message 字面 | `StatusCause` 是四条链路共用的配对 schema；扩形状换不来判据（验收只判 message） |
| D6 | 顺序：自引用 400 → 依赖未满足 409 → 12 格矩阵冲突 | 依赖是包外事实，先于「本包怎么落地」 |
| D7 | causes 只长在 requires 类错误上；其它 playbook 400 响应体逐字不变 | 「`requires` 省略时行为一致」的可验证形式 |
| D12 | 512 里 D 阶段写的 `requires.capabilities/plugins` + `status.resolved` 收窄成 `requires[]` + 回执 `dependencies` | 以 #534 冻结形状为准；`status.resolved` 不在 534 的验收里 |
| D13 | 锁只统计**清单仍在**的安装（`⨝ playbooks`） | 见 §5.3-1 |

### 5.2 审查（两位独立审查员，均未参与实现）

- `rev-correct`（正确性/回归）：10 项逐条核对，0 critical / 2 major / 若干 minor。
- `rev-test`（测试覆盖/简洁性）：十条验收逐条映射到判据（1-5、7、9 通过；6 弱、8 无证据、10 无机械判据），5 major。

**已修**：删清单导致能力永久不可解锁（D13）、卸载路径的 dryRun 判据未测、`capabilityConsumersOn` 缺「删另一个能力仍可删」的阴性用例（带变异实证）、多条未满足未测聚合、`MaxRequires` 上界内侧未测、`capability` 非字符串 / `minVersion` 负数与非整数 / `requires: null` 未测、e2e 的 `entryDoc` 默认值削弱旧包形状（改回省略）、e2e 重复用例 field 正则放松（收紧到 `[1]`）、`DeleteCapability` doc 滞后、注释配平越界（`namespace_credential.go` 已还原原文，改在 `playbook_uninstall.go` 配平）、报告数字与措辞不实（本版已改）。

**未修（有理由，交 root 裁决）**：

1. **卸载路径不查依赖锁**（rev-correct major-2）：`UninstallPlaybookRelease` 软删能力时不查 `capabilityConsumersOn`。**这是有意的**：issue 语义 3 把「卸载那个预案」定义为解锁的正路，若卸载也查锁就会与「A 依赖 B 的能力、必须先卸 A 才能卸 B」形成死锁，而仓库既有取向是允许悬空引用并由派生状态报出来（`resource_capability.go:557` 的 A16 注释）。后果：卸载 provider 时消费者的依赖会静默失效——与今天删能力的悬空语义一致，未新增风险。
2. **验收 10 无浏览器级分支断言**：两条 409 的 UI 分支只有代码 + `check-classes` / `check-i18n-keys` / `frontend` 套件（页面无 JS 错误）作为判据。要补得写 Chrome 用例（capacity 级），本轮未做。
3. **对照组是字段级而非金样逐字**：`TestPlanWithoutRequiresIsUnchanged` 比的是 `PlanResult` 的字段与 `Receipt` 的列表字段；e2e 比的是状态码与 `dependencies`。要真「逐字」，需要把改前产物落成金样文件（仓里没有这种设施）。

### 5.3 其他说明

1. 迁移块 7 是 task 提示里的落点在实现上等价于「回执也能存 dependencies」，之所以还要一张索引表：**「谁在依赖这个能力」必须在 SQL 侧可查**（否则删能力要拉全部回执逐条解析）。回执仍是审计原文，两者同事务写入。
2. `playbook_install_requires` 的 `install_id` 外键 `ON DELETE CASCADE`；`uninstalled_at` **不**删索引行 —— 锁判定用 `uninstalled_at = 0` 过滤，保留行让「这个 release 曾经依赖过什么」可回溯。
3. **注释密度棘轮**：本 PR 的注释行增量按目录逐条配平回基线（`internal/playbook` 35 行、`internal/store` 31 行、`internal/server` 21 行、`scripts/e2e/suites` 6 行、`internal/artifact` 1 行、`internal/store/v4test` 1 行），**基线文件一字未改**。代价：新代码注释从简（`requires.go` / `capability_lock.go` 只留 1 条最必要的），并删掉同目录几条纯复述函数名的旧注释（`errf` / `withDetails` / `IsCode` / `NoteCodes` / `MaxNameLen` 的单行 doc）。所有判据与「为什么」在本报告与 `.pi-legion/contract.md` 里。
4. `createPlaybook` 的 409 声明是 **main 上本来就有的**（slug_taken），本 PR 未改它——早期草稿误记为「新增」，已更正。
5. `installPlaybook` 的描述同时列了 `dependency_locked`（该端点不可达，只有 `DELETE …/capabilities` 会产生）与 `release_conflict`（可达但本次未提及）：前者是契约 §4 的要求，后者是既有文档的空洞（不在本 PR 范围）。

---

## 6. 不做（与 issue 的「不做」一致）

- 依赖的自动安装（`requires` 只校验，不触发装别的包）
- 跨命名空间依赖（解析与锁都只认本 ns）
- 版本范围语法（只有 `minVersion`）
- connector（E）
