# 矩阵内部并发与负载型假红：4 种配置 × 3 次全量的实测（#542）

> 结论先行：**（待填：采纳 / 否决）**

## 1. 问题

矩阵**之间**的互斥已由 #529/#530（`E2E_LOCK`）解决：同机两份矩阵会排队。但**同一份矩阵内部**
仍会出负载型假红，近三天实测三条且单跑全绿：

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
| **没有任何全局（跨 HTTP + Chrome）套件并发上限** | `parallel_runner.mjs` 旧实现：HTTP 类无条件全并行 |
| 现有唯一并发开关 `E2E_CONCURRENCY` 只管 Chrome 类（默认 4） | `parallel_runner.mjs:231` |
| 分片 = 每片一个独立矩阵（各自过锁） | `scripts/e2e/shard.mjs`、`E2E_TESTING.md:168` |

两条结论：

1. **不是跨套件抢数据库**。锁冲突只能发生在「同一套件内 server 连接 vs 套件自己的 Node 连接」
   之间；`busy_timeout` 已经设了 5s，所以 `database is locked` 的语义是
   **「等了 5s 还没拿到写锁」**，而不是「没设超时」。
2. 因此本实验要测的是 **CPU/IO 饱和度**（并发度），不是共享资源互斥。三套件的耗时（`capacity`
   单跑约 147s、`routing` 4.6s、`vars_ui` 2.6s，见 `shard.mjs:17-46` 的成本表）说明
   `capacity` 是长跑套件，而 `routing`/`vars_ui` 是**短跑但等待敏感**。

## 2. 实验设计

4 种配置 × 每格 3 次全量 `./scripts/e2e/run_all.sh --parallel`，**按轮次交错**（A B C D ×3）
而不是「一个配置连跑 3 次」：交错把机器漂移（越跑越热/越脏）摊到四种配置上。

| 配置 | 含义 | 环境变量 |
| --- | --- | --- |
| **A** 现状 | HTTP 类全并行 + Chrome 类并发 4（历史行为） | `E2E_PARALLEL=0` |
| **B** 封顶 `nproc/2` | 8 核 → 4 | `E2E_PARALLEL=$((nproc/2))` |
| **C** 固定 4 | 与核数无关的保守值 | `E2E_PARALLEL=4` |
| **D** 重套件串行收尾 | 27 套并行，`capacity`/`routing`/`vars_ui` 排最后串行单跑 | `E2E_PARALLEL=0` + `E2E_SERIAL_TAIL=capacity,routing,vars_ui` |

> **注：本机 8 核，所以 B ≡ C（都等于 4）。** issue 里的 A/B/C/D 是按 16 核写的；在本机 B 和 C
> 是同一个参数值。两格仍然各跑 3 次（不合并，避免"只跑赢的那个"）：它们的差异只在标签，
> 但 6 个样本对「cap=4 是否稳定」的结论更硬。

驱动脚本：`scripts/e2e/experiments/parallel-matrix.sh`（每跑完一格 append 一行到
`docs/reports/e2e-parallel-experiment.data.tsv`，中断不丢进度；开跑前硬等本机没有别的矩阵）。

每格记录：**总墙钟**（外层，含构建）、**矩阵内墙钟**（`parallel_runner` 自报）、失败套件数、
失败套件名、**失败套件单跑复验**（`run_all.sh <suite>`，串行）、开跑前 `load1`。

### 2.1 判定标准（issue 冻结）

- 若 B/C/D 任一配置 **3/3 全绿**且**墙钟增幅 ≤ 25%** ⇒ 采纳为默认（`E2E_PARALLEL` 默认 = 该值，`0` = 不限）。
- 否则 ⇒ **不改代码**，写数据 + 否决理由，issue 以 `won't fix（有数据）` 结束。

### 2.2 纪律（不做的）

- 不改任何套件断言或超时；不登记 `known-env-failures.json`（保持 `entries: []`）掩盖。
- 不只跑"赢的那个"配置。
- 不手工 `gh pr merge`。

## 3. 数据

（待填：由 `docs/reports/e2e-parallel-experiment.data.tsv` 生成的主表 + 逐格日志要点）

## 4. 判定

（待填）

## 5. 复现

```bash
export PATH=/usr/local/go/bin:$PATH
npm ci && make fe                 # 前端产物（dist-stamp 会硬校验新鲜度）
./scripts/e2e/experiments/parallel-matrix.sh          # 12 格，约 1 小时
# 或单格：
E2E_PARALLEL=4 ./scripts/e2e/run_all.sh --parallel
E2E_SERIAL_TAIL=capacity,routing,vars_ui ./scripts/e2e/run_all.sh --parallel
```
