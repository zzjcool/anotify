# e2e 判定器：补「整套崩」的容忍粒度（`case: "*"`）
<!-- snapshot-doc: 2026-11-19 #449 修复时的定格数字（自测 14 组 / 全量 e2e 28 套件、2358 断言），复算命令见 §4 / §6 -->

as_of: 2026-11-19 · area: e2e · 分支 `fix/e2e-suite-scope` · 基线 `origin/main` `85dc1f47`

## 0. 结论

1. **#449 那条红不是产品回归，也不是清单「记错了轮次」，是判定器的表达力缺口**：
   已知失败清单按 `case` 粒度登记，而那次 `ui_shift` 是**整个套件没有产出 results**
   （Chrome 渲染进程整批死亡，套件没跑到 summary）→ 判定器拿不到 case 名，只拿得到
   `null` → `match()` 逐字比较 `"interact-dialogs" !== null` → 判「清单之外」。
   **同一个环境性故障，按 case 死被容忍、按套件死被拦。**
2. 修法是把粒度补上，**不是放宽门禁**：清单条目允许 `"case": "*"`，语义是
   「这个 suite **整套没有 `results/<suite>.json`** 时才容忍」，**不**匹配任何具体 case 的失败。
   匹配规则见 §2 的表；写反了会被 §4 的【2】当场拦住。
3. 按「棘轮只许变小」把 `ui_shift / interact-dialogs` 那条**删掉**（换成 `ui_shift / "*"`）：
   同一次 CI 已经在提示「interact-dialogs 这次没失败 —— 删掉那一行」，说明那一行在故障
   真发生时根本没命中（故障的形态是整套崩），它是同一故障的**错粒度**登记。理由与代价见 §3。
4. 自测从 9 组扩到 **14 组**（10 组纯函数 + 4 组真 CLI），四组必需的判定与两条变异
   全部按预期（§4、§5）。`make check-affected-map` 绿；全量 `make e2e` 绿；PR #450 的
   `e2e（affected）` 也绿——而且**同一故障在那一轮 CI 上真的又复现了一次**
   （`ui_shift / 整套` 被容忍 + warning，见 §6.1）。

## 1. 现场与复现

### 1.1 #449 的 `e2e（affected）` 原文

```
❌ e2e 出现 1 条**清单之外**的失败：
  - ui_shift / 整套（没有 results）
##[warning]ui_shift / interact-dialogs 这次没失败 —— 删掉 known-env-failures.json 里那一行
```

清单当时有 `{"suite":"ui_shift","case":"interact-dialogs", ... "tracking":"#431"}`。
两行叠在一起就是缺口的完整证据：**case 级那一行没命中（所以提示可删），整套崩又没人能认领。**

### 1.2 本地复现（`origin/main` 的判定器 + 旧清单）

输入刻意还原现场：`frontend/source-actions` 一条已知失败（让 job 有内容）+ `ui_shift` 无 results。

```console
$ mkdir -p .e2e-bin/verdict-repro/{suite,case}/results
$ git show origin/main:scripts/e2e/ci-e2e-verdict.mjs > .e2e-bin/verdict-repro/old.mjs
$ git show origin/main:scripts/e2e/known-env-failures.json > .e2e-bin/verdict-repro/known-old.json
$ printf '%s' '{"suite":"frontend","passed":41,"failed":1,"failures":["source-actions — 偶发 case 子进程死亡"]}' \
    > .e2e-bin/verdict-repro/suite/results/frontend.json
$ # 调用方式与 §1.3 完全相同，只把脚本/清单换成上面两份基线副本，--selected "frontend ui_shift"

# 输出（origin/main 判定器 + 旧清单）：
::warning title=已知环境性失败（已登记）::frontend / source-actions —— runner 上 case 子进程偶发死亡（case.json 未落盘 / locator 4500ms 撞线），本地同代码 3–5s 通过（追踪 #431）
::warning title=已知失败清单可以缩小::ui_shift / interact-dialogs 这次没失败 —— 删掉 known-env-failures.json 里那一行（棘轮只许变小）

❌ e2e 出现 1 条**清单之外**的失败（必须修，或显式登记到 scripts/e2e/known-env-failures.json 并写理由）：
  - ui_shift / 整套（没有 results）
# exit 1
```

与 issue 里贴的两行**逐字一致** ⇒ 复现成立。

### 1.3 同一份输入的 after（新判定器 + 新清单）

```console
$ node scripts/e2e/ci-e2e-verdict.mjs \
    --results "$PWD/.e2e-bin/verdict-repro/suite/results" \
    --known   "$PWD/scripts/e2e/known-env-failures.json" \
    --selected "frontend ui_shift"
::warning title=已知环境性失败（已登记）::frontend / source-actions —— runner 上 case 子进程偶发死亡（case.json 未落盘 / locator 4500ms 撞线），本地同代码 3–5s 通过（追踪 #431）
::warning title=已知环境性失败（已登记）::ui_shift / 整套 —— 整套没有 results：runner 上 Chrome 渲染进程整批死亡；同一份代码本地 3–5s 通过（追踪 #431）

✅ e2e 判定：清单内 2 条（已容忍），清单外 0 条
# exit 0
```

反方向的证据（`*` 不是万能豁免）：`ui_shift` **有** results、只有 `interact-dialogs` 失败时：

```console
$ node scripts/e2e/ci-e2e-verdict.mjs ... --selected "frontend ui_shift"   # results 里有 ui_shift.json
::warning title=已知失败清单可以缩小::ui_shift / 整套 这次没失败 —— 删掉 known-env-failures.json 里那一行（棘轮只许变小）

❌ e2e 出现 1 条**清单之外**的失败（必须修，或显式登记到 scripts/e2e/known-env-failures.json 并写理由）：
  - ui_shift / interact-dialogs：interact-dialogs — 真超时
# exit 1
```

## 2. 缺口与匹配规则

### 2.1 改前（`scripts/e2e/ci-e2e-verdict.mjs`）

```js
const match = (suite, caseName) =>
	entries.find((e) => e.suite === suite && (e.case ?? null) === caseName);
```

- 整套没有 results → `match(suite, null)`：**只有** `"case": null` 的条目能命中；
  清单里写的是字符串 `"interact-dialogs"` → 永远不命中。
- 具体 case 失败 → `match(suite, "interact-dialogs")`：命中。

于是「同一故障的两种死法」被判成两种性质。缺的不是容忍，而是**登记粒度的词汇表**。

### 2.2 改后

```js
export function isWholeSuite(entry) {
	const c = entry?.case ?? null;
	return c === null || c === "*";
}

const match = (suite, caseName) =>
	entries.find(
		(e) =>
			e.suite === suite &&
			(caseName === null ? isWholeSuite(e) : (e.case ?? null) === caseName),
	);
```

### 2.3 规则表（这是本次要写死的语义）

| 清单条目 `case` | 套件**整套没有 results** | 套件内**具体 case 失败** | 套件跑绿 |
| --- | --- | --- | --- |
| `"<case 名>"` | ❌ 拦（#449 的缺口） | ✅ 容忍（仅同名 case） | ⚠️ 提示可删（stale，不红） |
| `"*"` | ✅ 容忍（warning 带 reason + tracking） | ❌ **拦**（`*` 不放行具体 case） | ⚠️ 提示可删（stale，不红） |
| `null` | ✅ 容忍（与 `"*"` 等价，历史写法） | ❌ 拦 | ⚠️ 提示可删 |

配套的两处硬化（同一类「条目存在但静默不生效」）：

1. **形状校验**：`validateEntries()` 校验 `entries[]` 的 `suite` / `case` / `reason` / `tracking`，
   形状不对直接 `exit 2`（数据错误），不进裁决里猜。`case` 拼错一个字符
   （例如 `"**"`）就是「永远匹配不上」，与本次缺口是同一种静默失败。
2. **拦下整套崩时给可执行提示**：`- ui_shift / 整套（没有 results）` 后面补一行
   `↳ 若这是已知环境问题：加 {"suite": "ui_shift", "case": "*"}（只覆盖「整套没有 results」，不覆盖具体 case）`。

## 3. 清单条目最终形态（以及为什么删掉那条 case 级）

`scripts/e2e/known-env-failures.json` 的 `entries` 现在是两条：

```json
[
  {
    "suite": "frontend",
    "case": "source-actions",
    "reason": "runner 上 case 子进程偶发死亡（case.json 未落盘 / locator 4500ms 撞线），本地同代码 3–5s 通过",
    "tracking": "#431"
  },
  {
    "suite": "ui_shift",
    "case": "*",
    "reason": "整套没有 results：runner 上 Chrome 渲染进程整批死亡；同一份代码本地 3–5s 通过",
    "tracking": "#431"
  }
]
```

**删掉 `ui_shift / interact-dialogs` 的理由**（不是顺手放宽，也不是顺手收紧）：

- 同一次 CI 的判定输出已经写明 `interact-dialogs 这次没失败`。也就是说故障真发生时，
  那一行**没有**命中 —— 症状是整套崩，不是某个 case 的断言失败。它是一条**错粒度**登记。
- 两条并存会让 `ui_shift` 同一环境故障占两行，其中一行永远 stale，棘轮会一直喊删；
  而且「两条并存」会让后来人以为这个 suite 是「case 和整套都被豁免」，比实际更宽。
- 代价是诚实的，写在这里：**若以后 Chrome 整批死表现为「有 results + 具体 case 失败」，
  判定会红**（§1.3 的第二段）。那时的正确动作是拿证据重新登记那一档，而不是当初就把
  `*` 写成「这个 suite 随便怎么失败都行」。`*` 不覆盖具体 case 是硬语义，由自测【2】钉住。

## 4. 自测（`node scripts/e2e/ci-e2e-verdict.mjs --self-test`）

从 9 组扩到 14 组：10 组纯函数 + 4 组真 CLI。四组必需项（任务卡要求的 4 条）标了【1】~【4】。

```console
$ node scripts/e2e/ci-e2e-verdict.mjs --self-test
  ✅ 【1】具体 case 失败 + 有对应 case 条目 → 容忍（exit 0）
  ✅ ★ 清单之外的失败 → 拦（exit 1）
  ✅ ★ 另一个套件的失败（清单没登记）→ 拦
  ✅ 【4】★ 整套没有 results + 只有具体 case 条目 → 拦（本次缺口就在这）
  ✅ 清单过期（这次没失败）→ 只提示，不红
  ✅ results 目录整体不存在 + 有选中套件 → 拦（不许假绿）
  ✅ 全绿 → 绿
  ✅ 【2】★ 具体 case 失败 + 只有 `*` 条目 → 必须红（`*` 不放行具体 case）
  ✅ 【3】★ 整套没有 results + 有 `*` 条目 → 容忍（绿，带 warning）
  ✅ 【5】`*` 条目过期（套件跑了且全绿）→ 只提示删，不红
  ✅ ★ CLI：清单之外的假失败 → exit 1（PR 会变红）
  ✅ CLI：清单内的具体 case 失败 → exit 0（job 绿）
  ✅ ★ CLI：整套没有 results + `*` 条目 → exit 0，且 warning 带 reason + tracking
  ✅ ★ CLI：有 `*` 条目但具体 case 失败 → exit 1

✅ e2e 判定自测通过（清单内容忍 / 清单外拦 / 清单过期只提示；`*` 只覆盖整套、不放行具体 case；CLI 端到端同样成立）
# exit 0
```

四组必需要点各自的断言强度（不是只数 exit code）：

| 组 | 输入 | 断言 |
| --- | --- | --- |
| 【1】 | `frontend` 里 `source-actions` 失败 + case 条目 | `ok=true`、`tolerated=1`、`blocking=0` |
| 【2】 | 同上，但清单只有 `frontend / "*"` | `ok=false`、`tolerated=0`、`blocking=1`（**必须红**） |
| 【3】 | `frontend` missing + `frontend / "*"` | `ok=true`、`tolerated=1`（`case=null`、`entry.tracking="#431"` 在 warning 里）、`blocking=0` |
| 【4】 | `frontend` missing + 只有 case 条目 | `ok=false`、`tolerated=0`、`blocking=1`（**必须红**） |

CLI 那四条走的是**真进程**（`spawnSync` 打真脚本 + 自带清单），用来证明「判据」之外
「接线」也成立：`::warning` 真的带 reason/tracking，整套崩真的 `exit 0`，`*` 真的不放行 case。
自测用的是 `.e2e-bin/verdict-selftest/known.json`（自带的清单），**不**钉死仓库清单内容 ——
否则 `entries` 按棘轮清空的那天，自测会白白变红。

## 5. 变异验证（两条都必须红）

### 变异 1：把 `case:"*"` 的匹配改回原样（只有 `null` 能覆盖整套）

```diff
 export function isWholeSuite(entry) {
 	const c = entry?.case ?? null;
-	return c === null || c === "*";
+	return c === null;
 }
```

```console
$ node scripts/e2e/ci-e2e-verdict.mjs --self-test
  ...
  ✅ 【2】★ 具体 case 失败 + 只有 `*` 条目 → 必须红（`*` 不放行具体 case）
  ❌ 【3】★ 整套没有 results + 有 `*` 条目 → 容忍（绿，带 warning）：ok=false（期望 true）；tolerated=0（期望 1）；blocking=1（期望 0）；tolerated[0].case=undefined（期望 null）；tolerated[0].tracking 丢了（warning 里必须带 tracking）
  ✅ 【5】`*` 条目过期（套件跑了且全绿）→ 只提示删，不红
  ...
  ❌ ★ CLI：整套没有 results + `*` 条目 → exit 0，且 warning 带 reason + tracking：exit=1（期望 0）
❌ e2e 判定自测失败 2 条
# exit 1
```

### 变异 2：让 `*` 也匹配具体 case（把整套登记变成万能豁免）

```diff
 	const match = (suite, caseName) =>
 		entries.find(
 			(e) =>
 				e.suite === suite &&
-				(caseName === null ? isWholeSuite(e) : (e.case ?? null) === caseName),
+				(caseName === null ? isWholeSuite(e) : isWholeSuite(e) || (e.case ?? null) === caseName),
 		);
```

```console
$ node scripts/e2e/ci-e2e-verdict.mjs --self-test
  ...
  ❌ 【2】★ 具体 case 失败 + 只有 `*` 条目 → 必须红（`*` 不放行具体 case）：ok=true（期望 false）；tolerated=1（期望 0）；blocking=0（期望 1）
  ✅ 【3】★ 整套没有 results + 有 `*` 条目 → 容忍（绿，带 warning）
  ...
  ❌ ★ CLI：有 `*` 条目但具体 case 失败 → exit 1：exit=0（期望 1）

❌ e2e 判定自测失败 2 条
# exit 1
```

两次变异都只改一处、跑完立即 `cp` 还原，`diff -q` 与备份逐字节一致（还原后再跑一次自测为绿）。

## 6. 门禁数字

| 门禁 | 命令 | 结果 |
| --- | --- | --- |
| 判定器自测 | `node scripts/e2e/ci-e2e-verdict.mjs --self-test` | 14/14 通过（10 纯函数 + 4 CLI），exit 0 |
| 改动选择自测 | `node --test scripts/e2e/affected.test.mjs` | 32 tests / 32 pass / 0 fail |
| 选择+判定门禁 | `make check-affected-map` | 绿（含上面两步） |
| 注解守卫 | `make check-annotations` | 绿（unbacked 300/300、unmarked 385/385，未上调基线） |
| 现场复现 | §1.2 / §1.3 的命令 | before exit 1（与 #449 逐字一致）→ after exit 0（2 条 warning） |
| 全量 e2e | `make e2e` | 绿：28/28 套件、**2358 断言通过 / 0 失败**，耗时 291.9s |
| CI（PR #450） | `e2e（affected）` | 绿：`✅ e2e 判定：清单内 2 条（已容忍），清单外 0 条` |

### 6.1 CI 上故障当场又复现了一次（这才是最硬的证据）

同一轮 CI（PR #450，`scripts/e2e` 改动 → 选择器保守地跑全量 28/28）里，`ui_shift`
**又整批崩了**（runner 上 Chrome 渲染进程再次死亡，没产出 results），而判定器按新语义
容忍并逐条打 warning：

```
##[warning]frontend / source-actions —— runner 上 case 子进程偶发死亡（case.json 未落盘 / locator 4500ms 撞线），本地同代码 3–5s 通过（追踪 #431）
##[warning]ui_shift / 整套 —— 整套没有 results：runner 上 Chrome 渲染进程整批死亡；同一份代码本地 3–5s 通过（追踪 #431）

✅ e2e 判定：清单内 2 条（已容忍），清单外 0 条
```

同一种死法，改前是 job 红（#449），改后是带 reason + #431 的 warning。这不是“推演出的语义”，
是**故障在改完以后真的再发生了一次、且被判对了**。

另一条 CI 事实（与本改动无关，记下来避免误判）：本轮 `quick（make ci）` 首跑红在
`cmd/anode/internal/runner` 的 `TestParentPositiveLeaseTimerRegistrationCrossingAdvance/register_at_61s`
——`rework2_test.go:197-209` 的 `waitUntil(..., 2*time.Second)` 墙钟预算在 runner 上被挤到 2.01s。
本地 `-count=8 -cpu=1` 全过（0.159s），`gh run rerun --failed` 即绿：既有测试的时序 flake，
与本次改动无关（本 PR 不碰 Go）。

全量 e2e 补记：首次运行在「生成前端产物」阶段就中止 —— 本 worktree 没有 `node_modules`，
`node scripts/check-wire-fields.mjs` 报 `ERR_MODULE_NOT_FOUND: Cannot find package 'yaml'`。
按仓库里已有 worktree 的做法把 `node_modules` 软链到主检出（`.gitignore` 已覆盖），重跑：

```console
==========================================
 汇总
==========================================
  总耗时: 291.9s
  断言通过: 2358
  断言失败: 0
  验收 authority: real 24 / explicit-mock 4（明细见每行 mode=；清单 .e2e-bin/results/_authority-manifest.json）
  ...
  ✅ frontend           42通过/0失败  208.5s  mode=real
  ✅ ui_shift           87通过/0失败  171.5s  mode=explicit-mock
==========================================
✅ E2E 全绿：28/28 个请求套件均达标（断言 2358 通过 / 0 失败，authority 清单覆盖 28 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 292.0s）
```

注意：本机全量里 `ui_shift` 是 `E2E_MOCK_ONLY=1` 的 explicit-mock 档（既有行为，本次未改），
所以它绿不代表 runner 上的 Chrome 整批死亡不再复现；本 PR 能保证的是**那种死亡被判成
「已登记容忍 + warning」，而不是「清单之外」。**

## 7. 遗留与后续

- 这套语义只覆盖「套件整体没有 results」；**套件跑到一半死、留下部分 results 且 `failed=0`**
  仍由 `parallel_runner` 的「passed+failed ≠ assertions.length / 缺 assertions[]」判 fail（它写的是
  `failures[]` 条目，走 case 粒度）。若以后观测到这种形态需要 `*` 覆盖，要单独加证据，不许顺手扩。
- `"case": null` 保留为 `"*"` 的等价写法（历史条目/外部脚本可能用），新条目一律用 `"*"`；
  等 `entries` 清空后可以连同这条兼容一起删。
- 清单里 `frontend / source-actions` 保持原样（本次没有它的新证据，不动）。
- **本地守卫对未跟踪文件是个假绿坑**（本 PR 第一轮 CI 就踩了）：`check-annotations` / `check-doc-links`
  都按 `git ls-files` 扫，新写的报告在 `git add` 之前根本不参与检查。本报告早稿里有一行
  直接调用 `.e2e-bin/verdict-repro/old.mjs`（现场从 `git show` 取出的临时副本、不入库）：本地
  那个文件恰好存在 → 绿；CI 上没有 → `REF_UNRESOLVED` 红。现在本地复验的口径是**把 `.e2e-bin/`
  移开再跑守卫**（等价于干净 checkout），而不是“本地跑过了”。
