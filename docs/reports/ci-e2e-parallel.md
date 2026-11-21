> **历史快照**：本文件是本次交付的取证记录，不随代码演进；行为以代码、测试与判定器为准。
> 数字全部来自下面两条 run 的日志（复算命令见 §6）。

# CI 的 `e2e（affected）`：14m21s → 4m51s（4 片并行，覆盖 29/29 不变）

## 1. 前后对比（**同一个 base commit `06488285`**）

| 指标 | 改造前 | 改造后 |
| --- | --- | --- |
| run id | [44874701960](https://github.com/zzjcool/anotify/actions/runs/44874701960)（PR #501） | [44875217385](https://github.com/zzjcool/anotify/actions/runs/44875217385)（PR #502） |
| 编排 | 一个 job，29 套**串行** | 计划分片 1 + 分片 4（并行）+ 总判定 1 |
| **`e2e（affected）` 阶段墙钟** | **14m21s**（06:19:39 → 06:34:00） | **4m51s**（06:25:58 → 06:30:49） |
| 套件执行（runner 内） | 785.4s（29 套相加） | 186.8 / 140.3 / 148.8 / 201.9s（各片 max = 201.9s） |
| 固定开销 | ~76s（checkout→构建→lint） | 计划 19s + 每片 ~56s（并行）+ 总判定 10s |
| 套件 / 断言 | 29 / 2457 | 29 / 2458 |
| 判定 | 清单内 0 / 清单外 0 | 清单内 0 / 清单外 0 |

两条 run 的**产品代码完全相同**（都基于 `06488285`）；差异只有 `.github/workflows/ci.yml` +
`scripts/e2e/**`（分片逻辑）。改造前的 run 是「原 workflow + `PI_E2E_FULL=1`」——`PI_E2E_FULL`
只为把选择器钉在「全量 29 套」上（改造后同样是 29 套），它不改变任何套件的执行方式。

**断言数 2457 → 2458 的差**：逐套件对比 29 套，28 套完全相同；唯一差异是 `orphans` 98 → 99
（该套件的计数逐 run 浮动：改造前 98、改造后 99、本机全量 98）。**没有任何套件少断言**。

## 2. 改了什么（只碰 CI 编排 + 分片逻辑，不改任何套件）

- `e2e-plan`：算 affected 选择（逻辑一字未改），再用 `scripts/e2e/shard.mjs` 切片。
  分片是**纯函数**（同一份 selected 永远切出同一份分片），按实测成本 + 每套件 ~4s 启动成本做
  「最长优先 + 迁移/交换」的局部改进；成本表过期只让某片慢一点，不影响覆盖。
  plan job 先跑 `node --test scripts/e2e/shard.test.mjs`（并集/互斥/纯函数/缺片可见）。
- `e2e-shard`（matrix，`fail-fast: false`）：每片一个 runner，`run_all.sh --parallel` 只跑自己那一片；
  结果 + `_shard.json`（本片本该跑什么）作为 artifact 上传。
- `e2e（affected）`（总判定）：`needs` 全部分片 + `always()`；合并各片 results 后调用**原来的**
  `ci-e2e-verdict.mjs`，`--selected` 是全量清单。任一分片 job 不是 `success` 也判红。
- 判定器、case 级/套件级两层重试、`known-env-failures.json`（entries 仍为空）**一字未改**。

## 3. 每组跑了哪些（合计 = 29）

| 分片 | 套件 | 断言 | runner 墙钟 |
| --- | --- | --- | --- |
| 片 1 | frontend | 43 | 186.8s |
| 片 2 | ui_shift | 87 | 140.3s |
| 片 3 | api_contract auth_flow capacity deeplink deploy edge_cases live_agent node_v4 orphans p2_guards passkeys password_login recovery_next routing vars_ui | 1207 | 148.8s |
| 片 4 | admin_flow cli_auth contract events_paging i18n i18n_coverage lang_hint passkey_enroll persistence security ux_regression_19_23 ws_protocol | 1121 | 201.9s |

总判定 job 的 `--merge` 输出：「期望 29 套 / 拿到 29 套」，四片的计划集与结果集逐片对齐（1/1、1/1、15/15、12/12）。

## 4. 两个反证（故意让它红，证明总判定不是装饰）

| 反证 | 做法 | 结果 |
| --- | --- | --- |
| 清单外失败 | `E2E_SUITE_BUDGET_PASSKEYS=1`（把 passkeys 的墙钟预算压到 1ms → 真实 timeout，且 timeout **不可**被清单容忍） | [44876017929](https://github.com/zzjcool/anotify/actions/runs/44876017929)：四个分片 job 全绿（run 步骤 `continue-on-error`），**总判定 job 红**：`⏱️ e2e 有 … 套件超出墙钟预算` + `❌ e2e 出现 … 清单之外的失败`（passkeys / timeout），exit 1 |
| 分片崩溃 | 分片 3 在跑套件前 `kill -9 $$`（不产出任何 results） | [44876021451](https://github.com/zzjcool/anotify/actions/runs/44876021451)：总判定 job **被执行**（`always()`，结论 `failure`，不是 `skipped`）：`分片汇总：期望 29 套 / 拿到 14 套` + 逐条列出缺失 15 套 + `分片 job 有失败（failure）⇒ 判红` |

两条探针都基于交付 HEAD，只加了注入手段（探针分支已删除）。清单外失败那条里分片 job 是绿的，
说明红**只可能**来自总判定 —— 这正是「汇总 job 真的会因分组红而红」。

## 5. 不变量（为什么这不是假绿）

1. **fail-closed 不变**：命中 `affected-map` / 共享 fixture / 判定器（`shard.mjs` 已登记为
   FULL_SURFACES；判定器与映射表本体照旧）⇒ 仍全量 29 套。
2. **少一片 = 红**：判定器按 `--selected`（全量清单）比对「结果集合 == 请求集合」，某片没上传
   就是那些套件没有 results ⇒ exit 1；计划 job 失败时总判定直接 exit 1（不把空清单当成「没事」）。
3. **空清单不退化成全量**：分片步骤显式拒绝空 `SHARD_SUITES`（`run_all.sh` 不带套件名 = 29 套全跑）。
4. **覆盖可复核**：每片落 `_shard.json`，总判定把「计划里的并集」与「拿到的结果」并排打印。

## 6. 复算

```bash
node --test scripts/e2e/shard.test.mjs                 # 分片逻辑自测（node --test 全绿）
node scripts/e2e/shard.mjs --from-plan plan.json --github-output   # 看某次选择怎么切
node scripts/e2e/ci-e2e-verdict.mjs --self-test        # 判定器自测（未改）
make ci && make e2e-affected                           # 本地门禁 + 本地全量（29/29 套；断言 2468 全通过）
```

CI 侧复算：上面两条 run 的 `e2e（affected · 计划分片）/ · 分片 N / e2e（affected）` 日志，
以及两条探针 run 的「总判定」日志。
