# Capability 删除与执行事实保留 · C2-D（父精准裁决）

落实已批准：用户资源删除后立即不再解析/显示，不新增in_use/finalizer，也不破坏已经钉住版本的任务。不是v3兼容，不引公共deletionTimestamp或独立Revision资源。

## 1. 最小内部 tombstone

- `capability_versions` 新增内部 `deleted_at INTEGER NULL`。`DELETE /.../capabilities/{name}` 在CAS事务中把该namespace/name所有**当前可见版本**标为deleted，不物理删除版本正文/ID/digest，不自动取消已授权任务。
- 该字段仅内部事实，不在metadata/spec/status投影给用户，不允许客户端写。不使用K8s finalizer/deletionTimestamp控制框架。
- 普通list、按name/latest/name@version解析、版本历史公开读取、foldKey寻址以及可部署目标只看 `deleted_at IS NULL`；删除后必须404/下游missing，不能继续把tombstone当当前资源。
- 普通live读取和trusted pinned-by-ID读取明确区分；已保留Task/attempt/部署执行引用通过私有ID得到原始不可变规格，不因name后来重建改指新行。Node内容获取只在有效部署/已授权任务的真实引用鉴权下允许，不变成任意已知digest读取隐藏代码入口。
- 移除删除者自己的live outgoing边投影；**引用它的其他资源next/targets不级联删改**，保留缺失引用供DownstreamResolved/源体检报告。已执行任务的emit使用所钉版本/路由事实，不能因为同名新资源重建而重取正文。

## 2. 重建同名与唯一性

- 把现全表UNIQUE(namespace_id,name,version)改为**当前live行**的精确唯一索引，predicate `deleted_at IS NULL`，允许历史tombstone和同名同版新行并存，但最多一条live。
- 重建得到新、永不复用的 `metadata.uid`，即使name/version/digest相同，也不能fold回tombstone或复用旧UID。版本分配/fold只针对live资源，显式同live版本异内容仍不可变。
- 旧任务pin旧UID/版本/digest；普通name@version只解析新live行。旧任务重试不能通过name解析后执行新资源内容。
- 新基线/布局validator检查deleted_at与**精确live唯一索引**及其有效predicate，不能把加尾列/错误partial/任意括号flatten视为等价。旧format4缺字段明确SchemaInvalid，不新增升级兼容。

## 3. CAS必须防删后重建 ABA

当前内部resource_version计数从1起、按行自增；它本身不能证明跨UID不重用。公共`metadata.resourceVersion`仍是**不透明字符串**，不得把裸内部counter作为可复用资源名字的完整CAS身份。

- 采用绑定不可复用UID与内部revision的opaque token（例如 `rv1:<uid>:<revision>`，具体统一编码由T1单一helper；客户端只保存/URL编码、不做Number运算）。无需新公开UID前置字段或If-Match协议。
- 任何name路径的PUT/PATCH/DELETE必须在同一事务校验**expectedUID + expectedRevision + namespace/name + live状态**；旧token即使counter恰与重建新行相同，也不能更新/删除新资源。
- 内部按不可变ID操作的revision计数可保留，但G3必须使用带UID绑定的CAS入口，不能先Get/checkUID再对name无条件更新；不能保留旧裸counter上层兼容入口绕过。
- 冲突仍409 resource_version_conflict，details.current.resourceVersion给**当前完整opaque token**；缺400 required、格式坏400明确invalid，不把内部计数当当前公共令牌返回。
- Source/Playbook/Node等可重建名字的资源遵同一token规则；Node根/config必须来自同一Node身份和底层revision，不各用不同token源。只需一致编码和原子预期身份，不新增全局控制框架。

## 4. 物理GC与引用

- 不论当前DDL的Task FK是否已存在，删除操作都不能物理破坏Task/attempt所pin的capabilityID；实际布局应在C3引用闭包中以FK或同事务严格引用检查防止误删。
- tombstone只有在Task/历史attempt/结果manifest/installed_from回执及实际部署引用均可安全清理后才可物理删除；仍有引用则保留，不借ON DELETE CASCADE摧毁历史。
- 能力部署内容共享digest，不能因删除一个逻辑版本而删其他live/pinned owner仍引用的内容。用户运行产物的7天TTL与能力spec生命周期不是同一件事。
- 禁止GC旧版本时按name删除重建的新live资源/边。所有物理清理按不可变ID和引用事务谓词执行。

## 5. 当前单writer精确授权

C2已有Source展示字段/资源CASowner基础上，父批准最小增补：schema.sql / migrate.go（tombstone+live唯一布局）；capability.go / ref.go（live解析、pinned事实、CAS删除/更新/创建）；已有C2统一CAS helper及必要新增resource_version.go；deployment读取过滤如直接引用需精确列在任务卡；Source/Node/config/Playbook现CAS调用的统一token编码/预期UID边界；相关v4test。

- 单writer，不改protocol/T3/前端或API文件，不在此实现整个G3HTTP；公共DTO后G3用该helper和原子entry。
- `internal/store/v4test/edges_test.go:40` 新UpdateNext期望CAS参数的精确调用适配允许，原断言不弱化；其他调用编译闭包必须明确列出，不大面积删旧测试。
- Source displayName既定field继续按已批准前置，不因Cap删除问题撤Source需求或把它塞config。
- 必测：deleted立即list/Resolve/版本公开404与missing；已pinTask仍旧spec；同name/version重建新UID+旧token409+新live不被GC；并发删/重建/修改CAS线性化；cap/displayName/next例外不变；live唯一约束、tombstone引用保持、shared digest不误删。

已有C2 worker仅写红tests/尚未production，正确暂停；扩具体任务卡后若仍活跃同writer继续，若safe退役确认后fresh单writer接已有测试，不盲并发。C1-R2已澄清不第三改代码，原194/Source前置/CAS新行为/最终全Go-contract-e2equick不削。
