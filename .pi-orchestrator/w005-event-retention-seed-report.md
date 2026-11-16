# 005 · `ANOTIFY_EVENT_RETENTION_DAYS` 改为「空库首次播种 system_config」

DONE 005

- 分支/基线：`main` @ `215dcf5`，本次提交 `bb53ee2 fix(server): 保留期 env 改为空库首次播种 system_config（#005）`
- 方案：按派发卡「冻结方案」的 **A′（env 只做首次播种，不做双源覆盖）** 实施，未越界改
  `scripts/perf/c/s6-capacity.sh`、未动 `schema.sql` / `migrate.go`、未删既有解析与 4 个断言。

## 产出文件

- `internal/store/config.go`：新增 `SeedSystemConfigIfEmpty`（单条 `INSERT OR IGNORE`，
  `resource_version=1`，`nil`/负数 → `ErrConfigInvalid` 且零写入，已有行返回 `false` 且不动一个字节）
- `internal/store/config_seed_test.go`：`TestSeedSystemConfigIfEmpty`（4 个子用例）
- `internal/server/config.go`：新增 `Config.EventRetentionDaysSet` + `retentionDaysSetFromEnv`
  （解析层 `retentionDaysFromEnv` 与既有 4 个测试一字未改）
- `internal/server/mux.go`：启动期接线移入 `reportEventRetention`（播种 + 报**生效值** + `source`）
- `internal/server/retention_seed_test.go`：`TestEventRetentionDaysSet`、`TestReportEventRetentionWiring`（6 个子用例）
- `DEVELOPMENT.md`、`docs/perf/capacity-and-backpressure.md`：环境变量表补「只播种」限定与
  `env_ignored` / `source` 说明

## 做了什么

1. **store 侧唯一写入口**：`SeedSystemConfigIfEmpty(ctx, cfg) (bool, error)`。
   单条 `INSERT OR IGNORE INTO system_config (id=1, ..., resource_version=1, updated_at=now())`，
   `RowsAffected()==1` 才是「确实播种了」。不写成「先查再插」：单行表（`id=1`）的冲突判定交给
   SQLite，多实例同时首次启动也只会有一次 `true`。`resource_version` 固定写 1 是刻意的 ——
   与「无行时 `GetSystemConfig` 报出的默认令牌」一致，首次 `PUT` 的前置条件不受影响，
   且「rv>1 ⇔ 至少 PUT 过一次」可以当证据用（见下面的 `source` 反推）。
2. **server 侧接线**：`NewApp` 在 `store.Open` 之后调用 `reportEventRetention(ctx, db, cfg)`：
   - 只有 `cfg.EventRetentionDaysSet` 才播种（未设置 = 不写库，保住「无行 = store 默认值」）；
   - 播种值 = `{events: cfg.EventRetentionSec(), task: store.DefaultRetentionTaskTTLSec,
     result: store.DefaultRetentionResultTTLSec}`（env 只描述 events）；
   - `seeded` → `server.retention.seeded_from_env`(Info)；已有行 → `server.retention.env_ignored`(Warn)；
     失败 → `server.retention.seed_failed`(Warn，**不阻断启动**)。
3. **日志改报生效值**：读 `db.GetSystemConfig(ctx).RetentionEventTTLSec` 决定
   `server.retention.configured` / `.permanent` 分支，`days` 由生效秒数反算（浮点，支持 PUT 写非整天数），
   新增 `source` 字段；`event` 名与 `events_retention_sec` / `est_bytes_per_event` / `hint` 全部保留
   （`permanent` 分支额外补了 `events_retention_sec`，纯新增）。读不到配置时多一条
   `server.retention.config_unreadable`(Warn)，然后按 store 默认值照常打那两条之一 ——
   不让「启动日志里没有保留期那一行」变成新的静默路径。

## 回答协调者的四个问题

### 1. 生效值现在从哪来、日志怎么体现 `source`

生效值**只**来自 `db.GetSystemConfig(ctx)`（= `system_config` 行；无行时是 store 的
`DefaultRetention*TTLSec`），这就是 `RunRetention` 每轮读同一份事实。日志里：

- `events_retention_sec` = 从库里读回的 `retentionEventTtlSec`（**不再**是 `cfg.EventRetentionSec()`）；
- `days` = `events_retention_sec / 86400`（浮点）；
- `source` 三值封闭：
  - `env_seed` —— 本次启动用 env 播下了行（`EventRetentionDaysSet` 且 `seeded==true`）；
  - `system_config` —— 生效值来自已有的行。判据：`resource_version > 1`（至少 PUT 过一次）
    **或** `events TTL ≠ store 默认值`（值只可能来自库里，含「上次启动播的种、这次 env 已撤」）；
  - `default` —— 无行且无 env：生效值就是 store 默认 30 天。

`GetSystemConfig` 刻意把「无行」与「有行」压成同一个结构体，所以只能靠上面两个可观测事实反推；
两者都不成立时按 `default` 报（值一致时来源对运维不再有影响），但**绝不**因为「反正值一样」就统一报
`default` —— 上次播的 90 天在重启后（env 已撤）必须报 `system_config`，这条有单测钉住。

### 2. 「env 已设但 `system_config` 已有行」时的行为与日志

行为：**一个字节都不改**（`INSERT OR IGNORE` 命中主键冲突 → `RowsAffected()==0` → 返回 `false`），
`resourceVersion` 不 bump，运行期 `PUT /v1/system/config` 的 CAS 完全不受影响。

真实二进制两次启动的原样输出（同一 DB，第一次 `env=90` 播种，第二次 `env=1`）：

```
{"level":"WARN","msg":"ANOTIFY_EVENT_RETENTION_DAYS ignored: system_config already has a row","event":"server.retention.env_ignored","days":1,"events_retention_sec":86400,"hint":"env 只在首次启动（system_config 无行）时播种；已有的行不会被覆盖，实际生效值见随后 server.retention.configured / .permanent 的 events_retention_sec；要改保留期请用 PUT /v1/system/config"}
{"level":"INFO","msg":"event retention configured","event":"server.retention.configured","days":90,"events_retention_sec":7776000,"source":"system_config","est_bytes_per_event":679.9,"est_bytes_per_event_1kb_payload":1908.7,"note":"..."}
```

注意 `env_ignored` 里的 `days/events_retention_sec` 是**你想设的值**（1 天），
紧跟的 `configured/permanent` 才是**生效值**（90 天）；hint 明确指向 `PUT /v1/system/config`。

其余四态的真机日志（同一二进制，四次启动）：

```
run1 空库 + env=90   : seeded_from_env(days=90)  + configured(days=90,  source=env_seed)
run3 重启 + env 撤   : configured(days=90, source=system_config)      ← rv=1 但值≠默认，靠值反推
run4 空库 + 无 env   : configured(days=30, source=default)            ← sqlite3: count(*)=0，确实没写库
run5 空库 + env=0    : seeded_from_env(days=0)   + permanent(source=env_seed, events_retention_sec=0)
                       sqlite3: 1|0|2592000|604800|1
```

### 3. 新增了哪些测试、各自钉住什么

`internal/store/config_seed_test.go · TestSeedSystemConfigIfEmpty`

1. **空库播种后读回一致且 rv=1** —— 往返一致性（本模块 §3.3）+ 播种行 `resourceVersion=1`
   + 播种后首次 `PUT(rv=1)` 仍成功（播种不能把库写成不可写）。
2. **幂等** —— 第二次用**不同的值**调用必须返回 `false` 且三个 TTL 一个都没变
   （若实现是 upsert，这条立刻红）。
3. **PUT 过之后 env 不覆盖** —— 先 `PutSystemConfig(7 天)` 再 seed(90 天)：值仍是 7 天、rv 仍是 2。
   这条就是「不做双源」的红线。
4. **nil / 负数报错且零写入** —— `errors.Is(err, ErrConfigInvalid)`，且之后再播种仍返回 `true`
   （= 表里确实没有留下半行）。

`internal/server/retention_seed_test.go · TestEventRetentionDaysSet`

- 未设置 / 空串 / 纯空白 → `false`；`"0"` / `"30"` / `"90"` / `"abc"` → `true`。
  钉住「显式 30」与「没配」是两件事（用数值猜会把前者误判成后者）。
  既有 `TestEventRetentionDaysEnvParsing` 的 4 条断言未改、未删。

`internal/server/retention_seed_test.go · TestReportEventRetentionWiring`（6 个子用例，抓 JSON 日志断言）

1. **env 显式 90 + 空库**：`seeded_from_env` 是 INFO 且 `events_retention_sec=7776000`；
   库里 events=90d、task/result=store 默认、rv=1；主日志 `source=env_seed`、`days=90`；
   `est_bytes_per_event` / `est_bytes_per_event_1kb_payload` 仍在。
2. **env=0 + 空库**：播成 0，走 `permanent` 分支（`source=env_seed`、`events_retention_sec=0`、
   `hint` 非空、`est_bytes_per_event` 在），且**不出现** `configured`。
3. **已有行 + env=1**：库里仍是 90 天、rv 仍是 2；`env_ignored` 是 WARN 且 hint 含
   `PUT /v1/system/config`；主日志 `source=system_config`、`events_retention_sec=7776000`
   —— **这条就是本 bug 的验收面**（日志必须说生效的 90 天，不能说 env 的 1 天）。
4. **env 未设 + 空库**：`source=default`、`events_retention_sec=2592000`；「没写库」的证据是
   之后 `SeedSystemConfigIfEmpty` 仍返回 `true`（只断言 rv==1 不够，播种行也写 rv=1）。
5. **env 未设 + 上次播种的行（rv=1、90 天）**：`source=system_config` —— 钉住 `source` 反推的边界。
6. **播种失败不阻断启动**：关库后调用，不 panic，出现 `seed_failed`(WARN, 带 error)，
   且仍有一条 `configured(source=default, 2592000)` 可读。

### 4. 有没有发现产品 bug（不改断言迁就）

**有 1 条，越界未改，需协调者授权**：`scripts/e2e/suites/capacity.mjs:474`

```js
H.eq("env=0 不改变 system_config：默认仍是 30 天", cfg0.json?.retention?.eventTtlSec, 2592000);
```

这条断言把**修复前的 bug 行为**写成了期望：它在 `extraEnv: { ANOTIFY_EVENT_RETENTION_DAYS: "0" }`、
harness 全新临时库的前提下断言 `system_config` 仍是默认 30 天。修好之后空库会播种成 0，
所以这里会变红（`got 0, want 2592000`）—— 这不是回归，是断言过期。
同一段还有三处注释同步过期：`capacity.mjs:24`、`445-446`、`472-473`
（「那个 env 在 v4 只剩启动日志」/「env 只影响启动日志，不改 system_config」）。
按任务约束（只碰计划授权的文件，`e2e` 是别人的 area）我**没有**动这个文件；
建议的改法是：把 474 行换成「env=0 播种后 eventTtlSec=0」，并保留后面的 PUT/对照组断言
（那两段仍然有意义：它们验的是「0 = 永久」而不是「env 生效」）。
`make e2e` 是协调者的门禁，请先授权再改，我在拿到授权后 1 分钟内能补上（含注释）。

## 验证输出（原样）

```
$ gofmt -l .
[gofmt exit=0]

$ go test ./internal/store/ -run 'Seed|Retention|Config' -count=1
ok  	github.com/zzjcool/anotify/internal/store	0.230s

$ go test ./internal/server/ -count=1
ok  	github.com/zzjcool/anotify/internal/server	52.962s

$ make contract
  A2/A4/A5/A6/A7/A8/A9/A11 通过（27 张表；3 张 kind 禁令、4 张内容标识、21 个中枢包、5 份 spec）
  信封/status/cursor/CAS 字段级检查通过（5 份文件 / 229 个 schema / 3 个具名 snake_case 例外）
  OpenAPI exact-five 通过（5 份文件 / 125 个 operation / 125 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）
  路由/handler/DTO 双向对账由 internal/server/openapi_parity_test.go 负责；信封字段规则由 scripts/check_envelope.py 负责
ok  	github.com/zzjcool/anotify/internal/server	2.055s
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
```

全量单测档（`make test` 的实体：`go test ./... -count=1`，88s）：

```
ok  	github.com/zzjcool/anotify/internal/store	6.177s
ok  	github.com/zzjcool/anotify/internal/store/v4test	24.235s
ok  	github.com/zzjcool/anotify/internal/server	53.579s
...
--- FAIL: TestInstallScriptRequiresToken / TestInstallScriptPreflightRejected / ...
FAIL	github.com/zzjcool/anotify/cmd/anode	84.628s          ← 基线红（sudo -n true / systemctl，本机无 sudo）
--- FAIL: TestDocsPageRendersFiveSpecFixturesFourLanguages
FAIL	github.com/zzjcool/anotify/internal/sitegen	0.905s        ← 基线红（sitegen docs，卡里已注明）
FAIL
```

**是否新增红：没有。** 两个红包都不依赖本次改动的代码 ——
`go list -deps ./cmd/anode | grep 'internal/(server|store)$'` 与
`go list -deps ./internal/sitegen | …` 都是空，所以它们不可能是本次改动引入的
（与卡里的基线红清单逐条对上：sitegen docs / cmd/anode 无 sudo / ws·runner flaky）。
`go vet ./...` 也是干净的（exit 0）。

## 遗留风险 / 未决问题

1. **`scripts/e2e/suites/capacity.mjs:474` 会红**（见问题 4），需协调者授权改断言 + 3 处注释。
2. **perf 数字口径需复测（不是回归）**：`scripts/perf/c/s6-capacity.sh` 每次 `pc_start` 都是全新
   dbdir → 现在每轮都会真的播种，b/c 段的 `retention=0/1` **终于真的生效了**。此前那两段实际按
   30 天运行（数字看似成立但前提是错的），所以 `docs/perf/capacity-and-backpressure.md` 里
   依赖该前提的容量/保留数字建议重跑一遍再引用。脚本本身按约束未动。
3. **无 schema / 无迁移改动**（`schema.sql`、`migrate.go` 未碰），所以老库不会被
   `ErrSchemaInvalid` 拒绝；`system_config` 表结构自 v4 baseline 起就存在。
4. **`env_ignored` 的语义是「本次启动没采用 env」而不是「env 永远无效」**：行为上等价
   （行存在就不再播种），但如果运维后来手工删掉 `system_config` 行，下次启动 env 会重新播种。
   这是 `INSERT OR IGNORE` 的必然结果，已写进 store 侧注释。
5. 部署脚本里若同时有「env 写 90 天」和「某处初始化时 PUT 30 天」，现在的效果以 PUT 为准，
   启动日志会给 `env_ignored` + `source=system_config` 两条线索（这正是期望的可追溯面）。
