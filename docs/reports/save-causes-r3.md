<!-- snapshot-doc: 2026-11-27 #645 第 3 轮交付时的本机实测定格（门禁 / e2e 的真实输出都在「验证输出」里给了可重跑的命令）；数字随代码变更会变，复跑以新输出为准 -->

# #645 保存失败 causes 标记 · 第 3 轮修复（PR #677 review r2）

分支：`pi-subagent/issue-645-r1`（基线 `origin/main` = `2607883b`，本轮提交 `1a5af6d5`）。
本文只记本轮（r3）对四条 review finding 的处理与取证；前两轮的实现与用例见之前的
`docs/reports/` 与 PR 正文。

## 做了什么

### MAJOR-1 用户可见文案：保存失败 / 试运行失败分口径

- `web/partials.js`：新增 `causeTitle(action, n)` + `CAUSE_TITLES{save,preview}`，
  标题只有一处拼装点；`causeReport(err, res, {action})` 按调用方给的动作取标题。
- `web/source-editor.js`：`error(box, err, action)` 把动作转交给 `causeReport`。
  保存分支传 `"save"`、`#btn-dry` 分支传 `"preview"`；两处 toast 都用同一个
  `A.causeTitle(...)`（保存分支原先自己拼 `t("common.save_failed_fields")`，试运行分支
  仍在拼未剪前缀的 `reason: message` —— 两处现在同口径）。
- 4 份 locale 各加一条 `common.preview_failed_fields`（试运行失败：{n} 处需要修改）。
- **可达性事实**（重要，避免又一次「宣称的验收点其实没被测到」）：今天的后端
  `previewInline` 不会回 `details.causes[]`。用真实服务端逐个探过：

  ```
  POST /v1/namespaces/default/sources/dry-run
    bad-config（url=http://x / schedule=bad / timeoutMs=-1）→ 200 {"targets":[],"pruned":[{"reason":"no_target"}]}
    bad-target-empty（capability 空）        → 200
    bad-target-match（types: []）            → 200 {"targets":[{"capability":"ghost",...,"reason":"downstream_missing"}]}
    ghost-target                             → 200
    payload-not-object                       → 400 value_invalid（无 causes）
    unknown-spec-field                       → 400 unknown_field（无 causes）
    targets-not-array                        → 400 BadRequest（无 causes）
  ```

  也就是说「试运行失败会显示保存失败横幅」在本轮之前是**潜在**缺陷（代码共用一个标题），
  不是今天能在浏览器里复现的路径。修法照做（`error()` 支持动作标题是 review 要求的接口），
  但覆盖落在单测：`causeReport(..., {action:"preview"})` 的标题行为 + 「接线」断言
  （试运行分支必须交 `preview`、保存分支必须交 `save`）。后端的 write 路径
  （`writeSourceError` → 400 + causes）本身没动。

### MAJOR-2 收敛到 (a)：删掉前端手抄剪裁表，信任后端剪裁

选 (a) 而不是 (b) 的理由：

1. 那张表已经**漂移**：前端写 `create/update/patch source:`，后端权威表是
   `create/update/delete/insert source:`（`internal/store/source.go`）；`create capability:`
   / `update capability: ` 后端根本不产出。
2. 该路径上的 message 早已被后端 `sourceConfigMessage` 剪过（`internal/server/api_status.go`），
   前端再剪是 no-op —— 保留「最小集合」等于保留第二份真相，下次改前缀又会静默失配。

改动：
- 删 `CAUSE_MESSAGE_PREFIXES` + `causeMessage()`；`causeText()` 只做 `String(x).trim()`，
  message 原样上屏（hint / 清单行 / JSON 原文一致）。
- 删掉恒真的 e2e 断言（`shown.includes('create source:') === false` 等两条负断言）。
- 换成**能失败**的等值比较：
  - 横幅标题 `assert.equal(report.title, '保存失败：' + causes.length + ' 处需要修改')`；
  - 清单每行 `assert.deepEqual(report.rows, ['<label>：<服务端 message>', ...])`；
  - toast 与横幅同口径 `assert.equal(toast.text, report.title)`；
  - 单测钉住「message 原样上屏，前端不再剪」：喂 `create source: invalid source config: …`
    必须逐字出现。

### MAJOR-3 分支覆盖 / 死代码

新增 `scripts/v4-causes.test.mjs`（10 条，vm 里真跑 `web/partials.js` + 最小真实 DOM）：

| 分支 | 处理 |
| --- | --- |
| `applyCauses` 的 `rest[]`（命中不到控件 → 不可点的 `<p class="cause-report-item">`） | **补用例**：`spec.run.entrypoint` 在只认 `params` 的 fieldMap 下落 rest[]，清单里是 `p`（不是 button、没有 `data-cause-jump`），文案逐字 `run.entrypoint：…` |
| `causeMapKey` 的前缀命中（`config.headers.X` → `config.headers`） | **补用例**：`config.headers.X-Api-Key` 与 `spec.targets[0].capability`（下标命中）各一条；能力页 e2e 早就走的是 `params.properties.project.pattern → params` 同一条分支 |
| `causeMapKey` 最长键优先 | **补用例**：`{config.schedule, config.schedule.tz}` 下 `config.schedule.tz` 必须标到 `f-tz`，不许退到 `f-schedule` |
| `focusCause` 的 `<details>` 展开 | **删掉**：能力弹层里的 `<details class="cap-adv">` 装的是 workdir/concurrency/env/requires/lifecycle，一个都不在 `CAUSE_FIELDS`；`cap-params` 在折叠块之外；源码编辑器整份没有 `<details>`。没有任何映射控件会落在折叠块里 —— 属未验证死代码 |
| `causeReport` 的 `n===0` 标题分支 | **删掉**：三个调用点（两个能力页 + 源码编辑器）都是 `if (res.causes.length)` 之后才画清单，0 条永远走各自的「reason: message」分支；标题改为无条件 `causeTitle(action, n)` |
| 同一控件多条 cause 合一行 / 清场还原 / 首条 focus | 补用例（合成一行 hint、`clearCauses` 还原页面原有 hint、`focusCause` 的 focus+scrollIntoView） |

`rest[]` 为什么没有 e2e：这两个页面上**没有任何真实 400 能产出映射不到的 field**。
能力写面的 `details.causes[].field` 只可能来自 `artifact.validateParamPatterns`
（全部是 `spec.params.*`）；源写面的每个 `sourcespec.Cause.Field`（`config.url` /
`config.schedule` / `config.timeoutMs` / `config.maxItems` / `config.headers` /
`config.secretRef` / `config.secret` / `config.envelope` / `targets[i]…`）都在
`web/source-editor.js` 的 `CAUSE_FIELDS` 里，唯一不在表里的是 `config.allowInsecure`，
而表单不提供这个键、也无法构造出库里的坏值。所以这条分支只能用单测喂真实形状的 cause
（`spec.run.entrypoint` + `bad_request`）；它是「后端将来新增字段」的安全网。

## 测试覆盖

- 单测 `scripts/v4-causes.test.mjs`：10 条（含接线断言），已接进 `make test-frontend`。
- 单测 `scripts/v4-frontend.test.mjs`：213 条（未改，全绿）。
- e2e `source-save-causes` / `capability-save-causes`：各 5 条行为断言（真实服务端 + 真 Chrome）。

## 验证输出

```
$ make fe
✅ 已写入 internal/server/dist/.source-stamp：输入面 70 个文件：文本 55 / 二进制 15（已计入 sha256）/ 超大跳过 0
   stamp 7b6f718f6a8a…

$ node --test scripts/v4-causes.test.mjs
# tests 10
# pass 10
# fail 0

$ node --test scripts/v4-frontend.test.mjs          # E2E_CHROME_PATH=<本机 Chrome>
# tests 213
# pass 213
# fail 0

$ node scripts/check-comment-density.mjs
✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线，新增必须在此刻被拦住

$ node scripts/check-test-discipline.mjs
✅ 五项全部与基线一致（t.Skip 登记 3/3 命中）—— 有改善请同步下调基线；新增必须在此刻被拦住

$ node scripts/check-annotations.mjs
exit=0

$ make check-dist-fresh
✅ dist 与源一致：stamp 7b6f718f6a8a…

$ node scripts/e2e/suites/frontend.mjs --case source-save-causes
PASS source-save-causes :: SAVE-CAUSES-01 real 400 carries per-field details.causes[] (spec.config.url, spec.config.schedule, spec.config.timeoutMs)
PASS source-save-causes :: SAVE-CAUSES-02 url / schedule / timeoutMs each get aria-invalid + the server message
PASS source-save-causes :: SAVE-CAUSES-03 banner + toast say 保存失败：N 处需要修改，rows are exactly 人话名：message and jump to their control
PASS source-save-causes :: SAVE-CAUSES-04 reason code stays in the raw-JSON fold, not in the human list
PASS source-save-causes :: SAVE-CAUSES-05 reopening the editor drops the previous round of marks (aria-invalid / hint / report)
  ✅ source-save-causes (5 behavior assertions)
[frontend] 1 通过 / 0 失败

$ node scripts/e2e/suites/frontend.mjs --case capability-save-causes
PASS capability-save-causes :: CAP-CAUSES-01 wizard marks #cap-params from a real 400 details.causes[] (1 cause rows)
PASS capability-save-causes :: CAP-CAUSES-02 clicking the reason row walks the wizard back to the step holding the control
PASS capability-save-causes :: CAP-CAUSES-03 reopening the wizard drops the previous round of marks (aria-invalid / hint / report)
PASS capability-save-causes :: CAP-CAUSES-04 the detail editor marks #cap-params from the same real 400 (one contract, two pages)
PASS capability-save-causes :: CAP-CAUSES-05 reopening the detail editor drops the previous round of marks (aria-invalid / hint / report)
  ✅ capability-save-causes (5 behavior assertions)
[frontend] 1 通过 / 0 失败
```

`make ci`：contract / gofmt / fe / vet / build / `make test-frontend`（含本轮新单测 10 条）/
`test-frontend-v4`（213/213）全绿；`make test`（Go）止于本机 macOS 环境性失败
（`cmd/anode/internal/listen` 的 unix socket 路径超长、`cmd/anode/internal/runner` 的
ETXTBSY/租约时序、`internal/artifact` 的 agent TUI `--cwd` 断言）。这三处与本次改动无关：
在父提交 `c27fcd5f`（未含本轮改动）上跑同样两个包，失败集合一致。

## 变异自证（新断言都能红）

单测（把实现改坏 → 跑 `node --test scripts/v4-causes.test.mjs`）：

| 变异 | 结果 |
| --- | --- |
| `rest[]` 不再 push（命中不到的 cause 被丢掉） | ❌ `not ok 5 - causeReport: 命中不到控件的 cause 落 rest[]…`（`# pass 8 / # fail 2`） |
| `causeMapKey` 取第一个命中而不是最长键 | ❌ `not ok 3 - applyCauses: 多个键都能命中时取最长的那个…`（`# pass 9 / # fail 1`） |
| `causeMapKey` 只认完全相等（删前缀/下标命中） | ❌ `not ok 2 - applyCauses: 前缀命中就能标…` + `not ok 5 rest[]…`（`# pass 8 / # fail 2`） |
| 不再 focus 第一条命中 | ❌ `not ok 10 - applyCauses: 第一条命中的 cause 把焦点带走…`（`# pass 9 / # fail 1`） |
| `causeTitle` 忽略 action（永远说保存失败） | ❌ `not ok 6 - causeReport: 标题跟动作走…`（`# pass 9 / # fail 1`） |
| 清单行重新叠 `create source: ` 前缀 | ❌ `not ok 7 - causeReport: message 原样上屏…`（`# pass 8 / # fail 2`） |
| 试运行分支的 `error()` 改回传 `save` | ❌ `not ok 8 - 接线：试运行分支交 action=preview…`（`# pass 9 / # fail 1`） |

e2e（改实现 → `make fe` + 重编 server → 跑 case）：

| 变异 | 实际输出 |
| --- | --- |
| `causeTitle` 把条数改成 `n+1` | `CASE_FAIL source-save-causes :: 横幅标题必须逐字是「保存失败：N 处需要修改」，实际 保存失败：4 处需要修改` |
| 清单行重新叠 `create source: ` 前缀 | `CASE_FAIL … 内容必须逐字是「字段人话名：服务端 message」…实际 ["URL：create source: 只允许 https（…）", "排期：create source: cron 表达式需要 5 段…", "单次超时：create source: 必须在 [1000, 120000] 毫秒内，得到 -1"]` |
| 保存分支的 toast 换成 preview 标题 | `CASE_FAIL … 保存失败的 toast 必须与横幅同口径，实际 {"text":"试运行失败：3 处需要修改","whole":"试运行失败：3 处需要修改复制详情×"}` |

三次变异都已还原并重跑：`# pass 213 / # fail 0`、两个 e2e case 全 PASS。

## 未决问题

1. **试运行（`#btn-dry`）的 causes 分支今天到不了**（见 MAJOR-1 的探测表）。文案修复与接线都已就位，
   但「试运行失败：N 处」只有在后端给 preview 路径加字段级校验之后才会有真实入口。要不要加那层
   校验是产品/契约决定（会让现在「200 + pruned」的草稿试运行变成 400），本轮没动后端。
2. 本机环境坑（与改动无关，写在这里省下一次踩）：`make fe` / 单测要能解析仓库根依赖目录里的 `yaml` /
   `playwright-core`）；`make ci` 的 contract 需要 PyYAML；`v4-frontend.test.mjs` 里 16 个
   `: native` 用例要 `E2E_CHROME_PATH` 指向本机 Chrome（`f3-driver.mjs` 只找 Linux 路径与
   `/usr/bin/google-chrome`）。
3. 注释棘轮：`web` 目录本轮净 0（新增 2 行注释由删掉的 2 行注释付账），总量仍 42889。
