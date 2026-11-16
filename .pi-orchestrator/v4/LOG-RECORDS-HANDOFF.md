# L1 · 真实节点日志记录补口（父冻结）

## 依据与范围

D1–D6/W6/A1不变。两个只读 scout（logs-wire-facts、logs-contract-facts）与第二意见 logs-seam-opinion 已确认：现 Node 原始 stdout/stderr 档案没有逐条时间/序号，不能回推 W6 的 `lines[{sequence,stream,text,at}]`。本补口提供真实采集，不弱化 W6、不编造旧数据、不恢复 v3。

现 Node 基线：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-anode`，`5a3dae3879662a7487f7248ab83af0776178a9cd`。由一个隔离的 Node-log centurion 单元交付，只有一个源码 worker；原 Node WT 和现 G2 执行 WT 不写入。

## 冻结决策 L1-R1～R8

### R1 事实与投影

- 每个 task/attempt 保存真实结构化记录，独立旁路索引与现 raw `stdout.log/stderr.log` 共存。raw 字节、权限、目录隔离和 D6 生命周期不缩减、不改写。
- `sequence` 从1开始、同 attempt 跨 stdout/stderr 严格递增，含义是**节点实际观察并串行记录的顺序**；不是脚本跨流因果顺序，也不能承诺两个 pipe 的真实写入先后。
- `at` 是节点采集该记录时的真实时间，RFC3339Nano；不取 attempt.finishedAt 回填，不校正为中枢时钟，不要求时钟倒退时 at 单调。
- `stream` 仅 stdout/stderr；`text` 是实际采集文字。LF分行，保留其他字节语义，不伪造尾LF；EOF残片作为真实观察的最后记录。为有界内存，单条文字段最多64KiB，超长行允许按实际采集片段形成多个记录（并不代表脚本写过额外换行）；原始完整字节仍以 raw 档案为准，注释/测试明确此展示记录语义。无效UTF-8按标准JSON文字投影，raw不受损。
- 不另设8MiB“只留头部索引”限额：那会永久丢掉最新记录且使tail/since错答。结构化档案和raw按现D6一起保留/GC；读请求与内存/回包有界，而非偷偷缩减档案保留。

### R2 写入故障与读取

- 两流共享 attempt 级串行索引写入者；raw继续原样写入。旁路索引的写入/解析失败不得通过tee的返回错误截断raw或改变真实进程退出事实。
- 错误锁存且有可检查的档案状态；不能把缺失/损坏/不支持的索引当真空日志。若有安全、真实的部分记录可返回，必须truncated=true；无法证明记录完整性时返回明确logs_unreadable，不造空成功。
- 读取运行中档案也必须有界、处理正在追加的末尾状态，不把半条JSON当完整记录、不重排序号。成功的空输出是存在的真实记录档案内0条，而非不存在档案。身份匹配、格式有效、无写入/解析失败的真实0记录索引允许成功`lines:[]`，不能仅因数量0判损坏；损坏/未知部分索引过滤后0不可冒充“无匹配记录”，须`logs_unreadable`。真实attempt档案不存在仍`no_local_archive`；档案存在但缺失/unsupported索引不可冒称过期或真空。
- 旧开发期raw-only档案不迁移、不补时间、不删除，明确报不可结构化读取。0forever不会授权删除用户档案。

### R3 shared protocol（仅现日志两帧补口）

- request保 `type/taskId/requestId/attempt?` 原义，新增 `tailLines`（非负，0或省略表示不加记录条数限制，上限5000）与 `sinceSeconds`（非负，0或省略表示不限时间）。字段camelCase、omitempty，显式attempt仍>=1。
- response新增 `lines: []LogLine`；LogLine恰有sequence/stream/text/at四字段。成功必须有非null数组及真实attempt；错误响应不伪造attempt或成功lines。
- 保现stdout/stderr/bytes/truncated/error传输字段与已有原文读取能力，不新增旧API/协议别名；它们不参与HTTP W6时间/顺序投影。`bytes`仍按当前T3定义为回包raw两流内容字节，不冒称完整档案大小。任何raw读取/帧级/记录级截断都必须传播truncated。
- 既有无档案错误 `no_local_archive` 保；新增明确不可读错误 `logs_unreadable` 与非法日志请求错误（闭集常量/校验）。只改日志帧，不动执行/cancel/emit/upgrade/身份协议。

### R4 查询与帧限额

- Node在真实结构化索引上，先以节点当前时间计算sinceSeconds阈值并过滤，再tailLines取尾，再按现1MiB帧上限取能完整传输的最新记录；必须先过滤后传输截断，中枢不能用raw事后拼造。
- response保持sequence的记录顺序、不重编号。任何遗漏匹配记录/截断/可读部分档案都令truncated=true；无匹配记录在档案有效时允许成功[]。不得因丢失/损坏索引返回[]“无匹配”。
- 大档案读取不一次load全文件；流式扫描+有界尾部缓存或等价真实索引读取，不能新增无限内存/裸等待。原raw读取超8MiB的more不能丢弃，原始档案本身继续完整。
- 完整JSON编码后确认<=1MiB，包括新记录、raw/metadata与Unicode转义开销，不能只按text长度估算。

### R5 Core/G3语义（本Node单元不写）

- 中枢按task_attempts事实选择**逻辑任务当前attempt**或真实上一attempt，并把明确attempt发送给其历史Node；不向当前Node问“本机latest”冒替全局latest，不跨节点自动挑最近有档案成功。
- `previous=true`而确实无上一attempt时：200 `{taskId,attempt:0,lines:[],truncated:false}`，仅此场景使用attempt0；不是节点离线/档案过期。
- 404任务不存在/跨namespace；400非法查询；503 node_offline；504 logs_timeout；410 logs_expired（真实attempt曾在且档案不在）；502 logs_unreadable/错误绑定或不合法响应。既有raw-only档案不可读不冒称“过期”。
- G2只实现真实historyNode/attempt/requestId/taskId关联、合法响应校验与转发；G3接HTTP路径/参数/status/五spec。中枢不落task_logs，不重排/重编号/补at、不自己按server时钟另过滤。
- FE mock的“latest有档案”实现若与全局当前attempt事实不一致，由后续专门授权的测试/mock单writer补齐真实契约，不弱化跨Node/离线/过期断言。本Trace门槛不掺入日志mock改动。

### R6 所有权与集成顺序

Node-log单元拥有：`internal/protocol/ops_frames.go`及日志直接相关测试/必要codec日志校验；`cmd/anode/internal/resultstore/**`（新索引writer/reader与测试）；`cmd/anode/internal/runner/attempt.go`与日志直接相关测试；`cmd/anode/internal/ops/ops.go`的HandleLogsRequest/日志编码与直接测试；必要App日志E2E测试、现D6调用接缝。其他执行/取消/调度/emit/artifact/upload/upgrade/store/Go依赖/Makefile/FE源码只读。需超scope先报父。

- 只有一个Node源码writer，manager只写专属契约/证据，不并发源码；fresh三角review附本R1–R8与刻意不做项。
- 先Node/protocol独立交付与机械验收；父在G2 writer安全冻结边界后整合共享Node/protocol补口，再授权G2消费。禁止活writer期间擅自改他的只读依赖或让G2造私有镜像。
- 本地owner-only提交允许；不push/MR/merge/rebase/reset/squash，不动.env/.pi/settings/.git/node_modules/用户数据，不删旧WT/session/失败日志。

### R7 测试与机械门禁

必须真实测试：两真实pipe/raw SHA字节一致、交织record序号单调、真实可解析at且不是finishedAt、EOF残片/空输出/长行/Unicode；append读取/异常旁路不损raw/损坏索引不可伪空；显式attempt/latest原节点能力、不越attempt；since→tail→frame边界及完整JSON<=1MiB；大量记录有界；raw more截断传播；无档案与raw-only档案错误区分；retention保活跃/7d/0forever；原Node真实执行/cancel/emit/artifact/upgrade测试不删不弱。

固定verify（Go环境direct/sum.golang.org/auto）：
1. canonical cwd/root/GOMOD与只读gofmt检查 + git diff --check。
2. go vet ./internal/protocol/... ./cmd/anode/...
3. go test ./internal/protocol/... ./cmd/anode/... -count=1
4. go test -race ./internal/protocol/... ./cmd/anode/... -count=1

长命令用受管工具/launch verify并完整日志和真实exit，socket/context/探针有界。相同根因最多2次失败后SAFE_BLOCKED精确checkpoint，不拿部分PASS宣整体。

### R8 刻意不做

不造过去日志时间/跨流因果，不全局日志落中枢，不新增collector进程/新socket，不改任务执行或重试事实，不重新定义result/emit/cancel/upgrade，不做旧数据兼容迁移、不自动清用户数据，不缩减raw档案，不把离线/缺索引/过期混成空成功。本单元不冒Node原交付重新完成或v4整体完成；原Node5a冻结门槛保留为历史证据，新补口另独立验收。
