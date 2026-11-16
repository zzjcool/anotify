---
id: 005
kind: bug
area: [server]
size: S
severity: P2
state: open
needs_recon: false
contract: []
depends_on: []
route: [worker]
gates: ["go test ./internal/server/... -count=1", "go test ./internal/store/ -run Retention -count=1"]
opened: 2026-11-16
---

# 005 · `ANOTIFY_EVENT_RETENTION_DAYS` 不生效 —— 只进启动日志，实际 TTL 来自 `system_config`

## 现象

`ANOTIFY_EVENT_RETENTION_DAYS` 在文档里被写成「events 保留天数」：

- `DEVELOPMENT.md:761`：`ANOTIFY_EVENT_RETENTION_DAYS | 30 | events 保留天数；0 = 永久保留`
- `docs/perf/capacity-and-backpressure.md:278`：同上，还写了钳位规则
- `scripts/perf/c/s6-capacity.sh:193,304,320,419`：靠它切「retention=0（永久）/ retention=1（一天）」

但代码里它**只被读来打日志**：

- 解析：`internal/server/config.go:408`（`Config.EventRetentionSec()`）、`config.go:254-285`
- 唯一消费点：`internal/server/mux.go:120-124`（启动时 `slog` 打 `events_retention_sec`）
- 真正决定 TTL 的是 `store.system_config`：`RunRetention` 每轮 `GetSystemConfig`（`retention.go:71`）
- 而 `system_config` 的**唯一写者**是 `PutSystemConfig`（`store/config.go:381`），
  调用点只有 HTTP `PUT /v1/system/config`（`resource_namespace.go:547`）。
  全仓没有「启动期用 env 播种 system_config」的路径（`grep system_config scripts/dev.sh cmd/devseed/*.go` 为空）。

⇒ 运维把 env 改成 90 天，重启后日志说 90 天，**实际仍在用 `system_config` 的默认 30 天**。

## 为什么现在才发现

这是 v4 换掉保留期配置来源时留下的**双轨残留**：配置搬到了 `system_config` + API，
env 那套没删、也没改成「无害别名」，文档和 perf 脚本继续按旧语义用它。

## 影响

1. **运维误判**：以为改了保留期，磁盘仍按 30 天删（或反之，以为永久保留却仍在删）。
2. **perf 结论的口径**（需 owner 确认）：`s6-capacity.sh` 的 b/c 段用 env 切 retention=0/1。
   若 env 实为空转，那两段的「retention=1」其实是默认 30 天 —— 40 天前的种子数据照样被删，
   所以**断言仍然通过、数字看似成立**，但「保留期 1 天」这个前提是错的。
   这对 `docs/perf/capacity-and-backpressure.md` 的结论口径是可追溯性缺口，不是数值必然错。

## 修法（二选一，建议 B）

| 选项 | 做法 | 代价 |
|---|---|---|
| A. 让 env 生效 | 首次 `Open` 且 `system_config` 无行时，用 env 播种 | 改 store（启动期写库 + 幂等）+ 新测试；与「保留期配置在 system_config」的双源问题**永久存在** |
| **B. 承认它已退役** | 改名/删除 env；文档改写成「保留期用 `PUT /v1/system/config`」；perf 脚本改用 `PUT`（或直接 `sqlite3` 写 `system_config`，perf 脚本已有库路径） | 文档 3 处 + 脚本 1 处；不动 schema、不动 API |

建议 B：保留期是**运行时可改**的配置（`GET/PUT /v1/system/config`），再留一个只在启动时生效的 env 就是双源。
若为了兼容已部署脚本而保留 env，至少要：启动日志显式写「本 env 已不生效，实际值来自 system_config」+ 文档标注 deprecated。

## 验收

- `grep -rn 'ANOTIFY_EVENT_RETENTION_DAYS'` 的每一处都对应「已退役」或「真的生效」，没有第三种。
- 若选 B：`make ci` 不新增红；perf 脚本仍能切 retention（改用 `PUT /v1/system/config` 或直接写库）。
- 若选 A：新增测试证明「首次 Open 用 env 播种」「已有行时 env 不覆盖」两条。

## 不做

- 不顺手改 retention 的默认值（30 天是用户拍板的，见 `internal/server/config.go:21-22`）。
