# 矩阵内部并发与负载型假红：4 种配置 × 3 次全量实测（#542）

snapshot-doc: 本报告是某台 8 核机器上一轮实验的定格快照（每格数字都对着 data.tsv 与日志可复算）；执行器改动以代码为准，复跑以新输出为准。

> **结论：不改默认值 —— 三种候选配置没有一个同时满足「3/3 全绿」和「墙钟增幅 ≤ 25%」。**
> 判定走 issue 的兜底分支：**不改代码**，本 PR 只含本报告与数据。
>
> 更关键的是：本次 14 次全量里出现的假红**不是「套件太多」造成的**。能复现的只有两条——
> `vars_ui` 的同一条 `delete → save` 竞态（5 次）与 `frontend` 的 f3 聚合层类型错误（1 次）；
> 前者在**空闲单跑**上根本不复现、在**慢机器单跑**上照样复现，后者**单 case 单跑是绿的**。
> 降并发只能降低触发概率，不能修掉竞态：`cap=4` 一共 8 个样本，**4 绿 4 红**。

| 配置 | 全绿 | 均值墙钟 | 增幅 | 判定 |
| --- | --- | --- | --- | --- |
| **A** 现状（不限并发） | 2/3 | 377.6s | — | 基线 |
| **B** 封顶 `nproc/2`（=4） | **1/3** | 411.9s | +9.1% | ✗ 全绿条件不满足 |
| **C** 固定 4 | **3/5**（前 3 次恰好 3/3） | 408.6s | +8.2% | ✗ 全绿条件不满足 |
| **D** 3 个重套件串行收尾 | **3/3** | 534.4s | **+41.5%** | ✗ 墙钟条件不满足 |

## 1. 问题

矩阵**之间**的互斥已由 #530（`E2E_LOCK`）解决：同机两份矩阵会排队。但**同一份矩阵内部**仍会出
负载型假红，近三天实测三条且单跑全绿：

| 套件 | 症状 | 单跑 |
| --- | --- | --- |
| `capacity` | 日志断言不符（全量并行时） | 绿 |
| `routing` | 9.1s 早退 `Error: database is locked` | 295/0 |
| `vars_ui` | 30s `locator.fill` 超时 / `waitForResponse` 15s 超时 | 74/0（3.2s） |

共同点：都发生在 `--parallel` 全量（30 套）时，机器 8 核 / 16G，窗口内 61 个 chrome + 7 个 `go test`。

### 1.1 侦察到的结构事实（决定这份实验该怎么读）

| 事实 | 证据 |
| --- | --- |
| 每套件独占 50 个端口段，无重叠 | `scripts/e2e/lib/harness.mjs:573-617` |
| 每套件每次启动一个独立 `mkdtemp` + `test.db`，**不跨套件共享数据库** | `harness.mjs:707-708,735` |
| SQLite `busy_timeout(5000)` + WAL + `_txlock=immediate` 已设 | `internal/store/store.go:101-103` |
| **没有任何全局（跨 HTTP + Chrome）套件并发上限** | `parallel_runner.mjs`（HTTP 类无条件全并行） |
| 现有唯一并发开关 `E2E_CONCURRENCY` 只管 Chrome 类（默认 4） | `parallel_runner.mjs:231` |
| 分片 = 每片一个独立矩阵（各自过锁） | `scripts/e2e/shard.mjs`、`E2E_TESTING.md:168` |

两条结论：

1. **不是跨套件抢数据库。** 每个套件自带一个 `test.db`，锁冲突只能发生在「同一套件的 server 连接
   vs 套件自己的 Node 连接」之间；`busy_timeout` 已经是 5s，所以 `database is locked` 的语义是
   **「等了 5s 还没拿到写锁」**（CPU 饥饿把写事务拉长到 5s 以上），不是「没设超时」。
2. 因此本实验测的是 **CPU/IO 饱和度**（并发度），不是共享资源互斥。`capacity` 单跑约 147s、
   `routing` 4.6s、`vars_ui` 2.6-3.1s（`scripts/e2e/shard.mjs:17-46` 的成本表 + 本次实测）说明
   `capacity` 是长跑套件，`routing`/`vars_ui` 是**短跑但等待敏感**。

## 2. 实验设计

4 种配置 × 每格 3 次全量 `./scripts/e2e/run_all.sh --parallel`，**按轮次交错**（A B C D ×3）
而不是「一个配置连跑 3 次」：交错把机器漂移（越跑越热/越脏）摊到四种配置上。

| 配置 | 含义 | 环境变量 |
| --- | --- | --- |
| **A** 现状 | HTTP 类全并行 + Chrome 类并发 4（历史行为） | `E2E_PARALLEL=0` |
| **B** 封顶 `nproc/2` | 8 核 → **4** | `E2E_PARALLEL=4` |
| **C** 固定 4 | 与核数无关的保守值 | `E2E_PARALLEL=4` |
| **D** 重套件串行收尾 | 27 套并行，`capacity`/`routing`/`vars_ui` 排最后串行单跑 | `E2E_PARALLEL=0` + `E2E_SERIAL_TAIL=capacity,routing,vars_ui` |

> **关键读数前提：本机 8 核，因此 B 与 C 是同一个参数值（都是 4）。** issue 里的 A/B/C/D 是按
> 16 核写的；在本机 B 和 C 只在标签上不同。两格仍各跑 3 次（不合并、不挑"赢的那个"），
> 于是 `cap=4` 一共拿到 **6 个样本** —— 这恰好是本次判定最有用的信息（§4.1）。

驱动脚本（§5 那段 shell 循环的完整版本）：每跑完一格 append 一行到
`docs/reports/e2e-parallel-experiment.data.tsv`（中断不丢进度、已存在的格子自动跳过）；
开跑前**硬等**本机没有别的矩阵（`chrome|headless_shell` 进程数 = 0 且没有别的 `parallel_runner.mjs`）。

每格记录：外层**总墙钟**（含构建）、**矩阵内墙钟**（`parallel_runner` 自报）、失败套件数、
失败套件名、**失败套件单跑复验**（`run_all.sh <suite>`，串行）、开跑前 `load1`。

### 2.1 判定标准（issue 冻结）

- B/C/D 任一配置 **3/3 全绿**且**墙钟增幅 ≤ 25%** ⇒ 采纳为默认。
- 否则 ⇒ **不改代码**，写数据 + 否决理由，issue 以 `won't fix（有数据）` 结束。

### 2.2 纪律（不做的）

- 不改任何套件断言或超时；不登记 `known-env-failures.json`（全程保持 `entries: []`）掩盖。
- 不只跑"赢的那个"配置；四格数据全在，且额外给唯一"合格"的 C 追加了 2 次（§3.2）。
- 不手工 `gh pr merge`。

## 3. 数据

主表（12 格，交错 3 轮；墙钟 = 外层秒）：

| 配置 | 第1次 | 第2次 | 第3次 | 均值 | 增幅 vs A | 全绿 |
| --- | --- | --- | --- | --- | --- | --- |
| A（现状） | 379.7 ✅ | 376.8 ✅ | 376.2 ❌ `vars_ui`（复验 **RED**） | 377.6s | — | **2/3** |
| B（cap 4 = nproc/2） | 418.8 ❌ `vars_ui`+`frontend` | 407.6 ❌ `vars_ui` | 409.3 ✅ | 411.9s | **+9.1%** | **1/3** |
| C（cap 4 固定） | 408.1 ✅ | 409.9 ✅ | 409.2 ✅ | 408.6s | **+8.2%** | **3/3**（前 3 次） |
| D（不限 + 3 套串行收尾） | 533.3 ✅ | 534.6 ✅ | 535.3 ✅ | 534.4s | **+41.5%** | **3/3** |

失败清单（5 次红、6 个失败套件实例）：

| # | 配置 | 失败套件 | 现场 | 单跑复验 |
| --- | --- | --- | --- | --- |
| 1 | B1 | `vars_ui` | `page.waitForResponse: Timeout 15000ms`（51/74 断言后） | ✅ 74/0，3.2s |
| 2 | B1 | `frontend` | `retained-security-permissions — TypeError: Cannot convert undefined or null to object`（272.8s，44/45） | ✅ 45/0，185.5s |
| 3 | B2 | `vars_ui` | 同 1 | ✅ 74/0，3.2s |
| 4 | A3 | `vars_ui` | 同 1 | ❌ 51/74，20.2s（同一现场） |
| 5 | C4 | `vars_ui` | 同 1 | ✅ 74/0，3.2s |
| 6 | C5 | `vars_ui` | 同 1 | ✅ 74/0，3.2s |

**`capacity` 与 `routing` 在全部 14 次全量（+6 次复验）里 0 失败。** issue 里那两条症状在本机
没有复现；本机能复现的假红只有 `vars_ui`（5/14 次矩阵）与 `frontend`（1/14 次矩阵）。

### 3.1 逐格原始记录

`.e2e-bin/experiment-logs/<配置>-run<第几次>.log`（构建产物，不入库）；
原始表格：`docs/reports/e2e-parallel-experiment.data.tsv`（列：配置 / 第几次 / UTC / 外层墙钟 /
矩阵内墙钟 / 退出码 / 失败套件数 / 失败套件名 / 单跑复验 / 开跑前 load1 / 核数）。

### 3.2 扩展样本：C 是唯一"看起来合格"的配置，所以只给 C 加样本

跑完 12 格时 C 正好 **3/3 全绿 +8.2%**（按字面判据它就合格了），而 B（**同一个参数值**）是
1/3。同一个配置出现 3/3 与 1/3 两种结论，说明 3 个样本不足以判定"稳定"。于是给 C 追加 2 次
（其余配置不动，避免"多跑直到某个配置变红"的选择性取样嫌疑——追加的对象是**唯一可能被采纳**的
那一格）：

| 配置 | 第4次 | 第5次 | 合计 |
| --- | --- | --- | --- |
| C（cap 4） | 407.4 ❌ `vars_ui`（复验 ✅） | 408.4 ❌ `vars_ui`（复验 ✅） | **3/5 全绿** |

**这一步是本次判定的转折点**：`cap=4` 的真实红率不是 0，而是 **4/8（B+C 合计：4 绿 4 红）**。
"3/3 全绿"只是在 ~50% 红率下连续命中 3 次的抽样结果，不能作为"这个配置稳定"的证据。

### 3.3 失败机理：5 次 `vars_ui` 失败是同一条竞态，不是"并发太高"

`vars_ui` 五次失败停在同一处（`scripts/e2e/suites/vars_ui.mjs:365-367`）：

```js
// 删一个 var：先点行内删除，再点保存，等真的 PUT …/yaml 回执
await page.locator('#node-vars-table tr[data-key="SOURCE_ROOT"] [data-act="vars-delete"]').click();
await saveAndWait(page, "#node-vars-save", "/yaml"); // ← 15s 内没等到 PUT
```

`saveAndWait`（`vars_ui.mjs:149-159`）等的是**真的 `PUT …/yaml` 网络回执**，15s 没来就抛。
所以失败语义是：**点了保存，但前端没有发出那次 PUT**，而不是「PUT 太慢」。紧邻的上一条断言
（"保存请求只打到 …/yaml（没有 /config 写）"）刚刚通过，说明同一套件的**上一轮**保存是正常的 ——
差别只在于这一轮多了一步「先点删除、再点保存」。

同一套件的空闲基线（本机实测）：

| 场景 | 结果 |
| --- | --- |
| 空闲单跑 ×6 | **6/6 绿**，每次 3.0-3.1s |
| 矩阵内单跑复验 ×4（B1/B2/C4/C5） | 绿，3.2s |
| 矩阵刚结束后**立刻**复验 ×1（A3） | **红**，51/74，20.2s |

三个数字指向同一件事：这条竞态**只在机器慢的时候**被放大（20.2s vs 3.1s = 6.5×）。机器慢既可能
来自矩阵并发（B1/B2/C4/C5），也可能来自**矩阵刚结束、`load1` 6-8 还在衰减**的复验窗口 ——
A3 的复验就是在这种状态下跑的（所以那一格不能记成"负载型假红、单跑即绿"）。**降并发只是把这
个窗口的概率压小，没有触碰竞态本身**：C5 是在 `E2E_PARALLEL=4`（同时最多 4 个套件）下依然复现的。

`frontend` 那一次是另一类问题：f3 的**单 case 单跑是绿的**（复验 45/0；单 case 证据
`scripts/e2e/fixtures/f3/screenshots/f3/runs/frontend-*/…/full.log` 里是
`PASS retained-security-permissions` + `CHILD_EXIT=0`），失败发生在**把 case 结果聚合成套件结论**
的那一层，报的是一个 `TypeError: Cannot convert undefined or null to object`。这是**诊断质量问题**
（case 在慢机器上没产出结果时，聚合层没有说"哪条 case 没结果"，而是抛了个类型错误）。
两类失败都与"套件数量"无关。

## 4. 判定

### 4.1 三种候选全部不满足冻结判据

| 配置 | 3/3 全绿？ | ≤25% 墙钟？ | 结论 |
| --- | --- | --- | --- |
| B（cap `nproc/2`） | ✗（1/3） | ✓（+9.1%） | 不采纳 |
| C（cap 4 固定） | ✗（**3/5**；前 3 次曾是 3/3） | ✓（+8.2%） | 不采纳 |
| D（3 套串行收尾） | ✓（3/3） | ✗（**+41.5%**） | 不采纳 |

D 的超标是**结构性**的，不是抖动：三个重套件串行单跑要额外花约 153s（`capacity` 单跑就 147s），
535.3 - 376.2 ≈ 159s，与实测一致。也就是说 D 每跑一次全量就多付 2.6 分钟，换来的是
"把三条已知会红的套件挪出并发窗口"——但它买不到 `vars_ui` 的稳定（见 §3.3，C5 在 cap=4 下仍红）。

### 4.2 否决理由（四条，都带数字）

1. **封顶没有降低假红率，只是把它的分布换了个样子。** `cap=4`（B+C 合计 8 样本）= **4 绿 4 红**；
   现状 A = 2/3；D = 3/3 但 +41.5%。没有任何一档在"稳定"这个维度上比现状好到值得为它付代价。
2. **能复现的假红与并发度无关。** 5 次 `vars_ui` 失败是同一条 `delete → save` 竞态（前端没发出
   那次 PUT）：空闲单跑 6/6 绿，慢机器单跑也红（A3 复验），`cap=4` 下照样红（C5）。
3. **`capacity` / `routing` 在本机 14 次全量里 0 失败**（各 14 次套件执行）。issue 里
   `database is locked` 的前提（`busy_timeout` 未设）经核实不成立（`store.go:101` 已是 5000ms），
   本机无法复现该症状，因此也无法用它论证并发封顶。
4. **代价是每次都付的。** cap=4 → +8.2~9.1%；D → +41.5%（约 153s/次的结构性成本）。
   为一个未被证实有效的手段，让每次全量多花 30-150s，不划算。

### 4.3 因此不改代码

- **不新增/不改并发默认值**：本 PR 不含产品代码改动（`scripts/e2e/parallel_runner.mjs` 与
  `base` 逐字一致，已用 `git diff --stat` 核对）。实验用的两个开关放在
  报告附录 A（默认 0 = 逐字等于历史行为），需要复现时把那段 diff 存成文件 `git apply` 即可，
  不落在主干执行路径上。
- 下一步该修的是**两条真实缺陷**，而不是并发度（建议各自单独开 issue）：
  1. `vars_ui` 的 `delete → save`：点保存后前端没发 `PUT …/yaml`。修法方向 = 在点击前等
     「表格 dirty / 保存按钮可用」，点完后断言 `writes.length` 增长（不是抬 15s 超时）。
  2. `frontend` 的 f3 聚合层：`TypeError: Cannot convert undefined or null to object`。
     修法方向 = case 结果缺失时报「哪条 case 没产出结果」，而不是抛类型错误。

## 5. 复现

```bash
export PATH=/usr/local/go/bin:$PATH
npm ci && make fe                     # 前端产物（dist-stamp 硬校验新鲜度）
git apply e2e-parallel.patch          # 附录 A 的 diff，存成这个文件：E2E_PARALLEL + E2E_SERIAL_TAIL

# 12 格（按轮次交错 A B C D；每格 ~6.5 分钟 + 失败复验）
for round in 1 2 3; do
  for cfg in A B C D; do
    case $cfg in
      A) P=0; T="" ;;
      B) P=$(( $(nproc) / 2 )); T="" ;;
      C) P=4; T="" ;;
      D) P=0; T="capacity,routing,vars_ui" ;;
    esac
    E2E_PARALLEL=$P E2E_SERIAL_TAIL=$T timeout 2400 ./scripts/e2e/run_all.sh --parallel \
      > .e2e-bin/experiment-logs/$cfg-run$round.log 2>&1
    # 失败套件单跑复验：./scripts/e2e/run_all.sh <suite>
  done
done
```

> 本 PR **不**把上述循环脚本与补丁作为文件交付：仓库的注释密度棘轮（`make ci` 的
> `check-comment-density`，`TOTAL_OVER`）要求仓库总注释行不高于基线 43300，而当前正好卡在
> 43300 —— 新增带注释的脚本目录会破门禁。实验代理由上表十几行 shell 完全覆盖，补丁见附录 A。

## 6. 验收对照

| issue 验收 | 状态 |
| --- | --- |
| 1. 4 配置 × 3 次数据表，含失败套件名与单跑复验 | ✅ §3（+C 追加 2 次，§3.2） |
| 2. 采纳的配置连续 3 次 0 失败 | ⛔ 无配置可采纳（§4.1） |
| 3. 未采纳的配置也有数据 | ✅ 四格各 3 次全在，C 另加 2 次 |
| 4. `E2E_PARALLEL=0` 能恢复旧行为 | 不适用（未采纳 ⇒ 主干行为不变；补丁里的默认值也是 0 = 不限） |
| 5. `make ci` exit 0；`make e2e-affected` 绿；known-env 保持空 | ✅ 见 §7 |
| 6. 不改代码则写明否决理由与数据 | ✅ §4 |

## 7. 门禁

- `make ci` → exit 0（本树与 `079cf255` 相比**没有任何代码改动**：5 个受管文件里 4 个是
  `docs/reports/`，1 个是被 revert 回基线的 `scripts/e2e/parallel_runner.mjs`）。
- `make e2e-affected` → 见 PR 描述（纯文档改动时它会判「e2e 无关」；本次另跑了全量 `make e2e` 作实据）。
- `scripts/e2e/known-env-failures.json` 全程 `"entries": []`，未登记任何新例外。

## 附录 A：实验开关的补丁（`E2E_PARALLEL` / `E2E_SERIAL_TAIL`）

默认 0 = 逐字等于历史行为；不采纳它，所以它**不在**主干。复现时 `git apply` 即可：

```diff
diff --git a/scripts/e2e/parallel_runner.mjs b/scripts/e2e/parallel_runner.mjs
index 7de14743..a06b9e5e 100644
--- a/scripts/e2e/parallel_runner.mjs
+++ b/scripts/e2e/parallel_runner.mjs
@@ -32,6 +32,9 @@
  *
  * 环境变量：
  *   E2E_CONCURRENCY  Chrome 类套件并发上限（默认 4）
+ *   E2E_PARALLEL     全局套件并发上限（跨 HTTP/Chrome 两类）。默认 0 = 不限；
+ *                    只影响套件并发度，不影响机器级互斥（#529）与分片语义（#542）
+ *   E2E_SERIAL_TAIL  逗号分隔的套件名：这些套件排在最后**串行**单跑（#542 实验用）
  *   E2E_RESULTS_DIR  JSON 结果输出目录（默认 .e2e-bin/results）
  *   ANOTIFY_BIN / DEVSEED_BIN / ANODE_BIN  二进制路径（由 run_all.sh 设置）
  *   ANOTIFY_VAPID_*  VAPID 密钥（由 run_all.sh 设置）
@@ -229,6 +232,36 @@ const SUITE_REGISTRY_SIZE = 30;
 const RESULTS_DIR =
 	process.env.E2E_RESULTS_DIR || path.join(ROOT, ".e2e-bin", "results");
 const CONCURRENCY = parseInt(process.env.E2E_CONCURRENCY || "4", 10);
+// #542：全局（跨 HTTP/Chrome 两类）套件并发上限。
+//   0 / 不设 = 不限 ⇒ 与历史行为逐字一致（HTTP 类全并行 + Chrome 类并发 CONCURRENCY）。
+//   为什么需要它：矩阵**内部**的并发是独立于 #529 机器级互斥的第二层压力 ——
+//   同机 8 核上 14 个 HTTP 套件同时起，capacity/routing 会撞 sqlite 锁与 CPU 抖动。
+//   上限必须只改「同时跑几个套件」，不许改锁、分片、预算、重试与判分。
+const PARALLEL_LIMIT = (() => {
+	const raw = process.env.E2E_PARALLEL;
+	if (raw === undefined || raw === "") return 0;
+	const v = Number(raw);
+	if (!Number.isInteger(v) || v < 0) {
+		console.error(`E2E_PARALLEL 必须是非负整数（0 = 不限），现在是 '${raw}'`);
+		process.exit(2);
+	}
+	return v;
+})();
+// #542 实验用：这些套件排到最后串行单跑（默认空 = 不启用）。
+// 名字必须真实存在：静默丢弃一个拼错的套件，会让「重套件已串行」变成一句空话。
+const SERIAL_TAIL = (() => {
+	const list = (process.env.E2E_SERIAL_TAIL || "")
+		.split(",")
+		.map((s) => s.trim())
+		.filter(Boolean);
+	const unknown = list.filter((s) => !ALL_SUITES.includes(s));
+	if (unknown.length > 0) {
+		console.error(`E2E_SERIAL_TAIL 里有未注册套件: ${unknown.join(", ")}`);
+		console.error(`已知套件: ${ALL_SUITES.join(", ")}`);
+		process.exit(2);
+	}
+	return list;
+})();
 // #488：套件级重试的墙钟上限。再重试一次就会撞上 job 超时（= 赌整份 artifact 丢掉）。
 const RETRY_WALL_LIMIT_MS = (() => {
 	const v = Number(process.env.E2E_RETRY_WALL_LIMIT_MS);
@@ -443,37 +476,67 @@ function attemptDetailLines(s) {
 // ---------- 并行执行 ----------
 async function runParallel(suites, runStartedAt) {
 	// 分组
+	const tail = SERIAL_TAIL.filter((s) => suites.includes(s));
+	const tailSet = new Set(tail);
+	const head = suites.filter((s) => !tailSet.has(s));
+
+	const results = await runParallelGroup(head, runStartedAt);
+
+	// #542 实验配置 D：重套件排在最后串行单跑（与在并行组里跑是同一份判分口径）。
+	if (tail.length > 0) {
+		console.log(`▶ 串行收尾（E2E_SERIAL_TAIL）：${tail.join(", ")}`);
+		for (const name of tail) {
+			const t0 = Date.now();
+			const r = await runSuite(name, runStartedAt);
+			results.push(r);
+			console.log(
+				`  ${r.status === "pass" ? "✅" : "❌"} 串行收尾 ${name} ${((Date.now() - t0) / 1000).toFixed(1)}s`,
+			);
+		}
+	}
+	return results;
+}
+
+async function runParallelGroup(suites, runStartedAt) {
 	const httpSuites = suites.filter((s) => HTTP_SUITES.includes(s));
 	const chromeSuites = suites.filter((s) => CHROME_SUITES.includes(s));
 
+	// 全局上限（0 = 不限）与 Chrome 类上限**叠加**：两者都只是「同时跑几个套件」。
+	const globalSem = PARALLEL_LIMIT > 0 ? new Semaphore(PARALLEL_LIMIT) : null;
 	const sem = new Semaphore(CONCURRENCY);
 	const results = [];
 
+	const run = async (name, chrome) => {
+		if (globalSem) await globalSem.acquire();
+		try {
+			if (!chrome) return await runSuite(name, runStartedAt);
+			await sem.acquire();
+			try {
+				return await runSuite(name, runStartedAt);
+			} finally {
+				sem.release();
+			}
+		} finally {
+			if (globalSem) globalSem.release();
+		}
+	};
+
 	// HTTP 类全并行 + Chrome 类信号量控制
 	const tasks = [];
-
 	for (const name of httpSuites) {
 		tasks.push(
-			(async () => {
-				const r = await runSuite(name, runStartedAt);
+			run(name, false).then((r) => {
 				results.push(r);
 				return r;
-			})(),
+			}),
 		);
 	}
-
 	for (const name of chromeSuites) {
 		tasks.push(
-			(async () => {
-				await sem.acquire();
-				try {
-					const r = await runSuite(name, runStartedAt);
-					results.push(r);
-					return r;
-				} finally {
-					sem.release();
-				}
-			})(),
+			run(name, true).then((r) => {
+				results.push(r);
+				return r;
+			}),
 		);
 	}
 
@@ -821,8 +884,12 @@ async function main() {
 	console.log(
 		serial
 			? `模式: 串行（${suitesToRun.length} 个套件）`
-			: `模式: 并行（HTTP 类全并行, Chrome 类并发 ${CONCURRENCY}）`,
+			: `模式: 并行（HTTP 类全并行, Chrome 类并发 ${CONCURRENCY}, ` +
+					`全局并发上限 ${PARALLEL_LIMIT > 0 ? PARALLEL_LIMIT : "不限"}）`,
 	);
+	if (SERIAL_TAIL.length > 0) {
+		console.log(`  串行收尾（E2E_SERIAL_TAIL）: ${SERIAL_TAIL.join(", ")}`);
+	}
 	// 验收 authority 预告：先声明本轮的 real / explicit-mock 分工（#338 裁决）。
 	const mockList = suitesToRun.filter((s) => EXPLICIT_MOCK_SUITES.has(s));
 	const realCount = suitesToRun.length - mockList.length;
```
