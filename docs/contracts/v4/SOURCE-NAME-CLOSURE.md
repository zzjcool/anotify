# B 唯一真实 route 源名红测：精确生产修复授权

> 迁移自 .pi-orchestrator/v4/SOURCE-NAME-CLOSURE.md（2026-11-19，协调者工作区不再入库）
> snapshot-doc: 2026-11-19 迁移时的原件定格（文中数字是当时契约定格/实测，不是「现在是多少」的承诺）

父已核真实 `/tmp/anotify-v4-delivery-b-gate3-test.log`：有效原始包的source物化名确为route，安装实际生成Capability/Source/InstallReceipt；同名directCreate拒绝。其余原store运行与外v4test/auth/push通过，不能因该红而删断言。现source.go仅trim后拒空/route，安装create/reuse和独立Plan/Render都应遵现相同创建规则。

## 最小共同校验

- 新增共享 `internal/sourcespec/name.go` 的一个name validator（名字按实际包风格），仅复用**已有trim-empty与字面route**规则。原Name本身不被隐式改写；不加github/cron/http/manual旧CAP禁名、新Regex、大小写归一、Namespace/targets/credential限制。
- 现 `store.ErrReservedEventSource`（event.go:47，`"route" 是保留来源名（剪枝记录专用）`）必须维持现文本和errors.Is身份契约。可把同一sentinel定义于shared sourcespec并在store保留现导出变量的同指针别名；允许`event.go`仅相应import/现sentinel定义这一处，不改事件行为或扩展错误API。不是恢复历史兼容入口。
- `internal/store/source.go` directCreate用该validator替代同样的现局部判断，NamespaceID及原空名字错误语义不改；route经Render包装和Install返回仍errors.Is(ErrReservedEventSource)。
- `internal/playbook/render.go` 在Source实际name被values物化后调用同validator，让Render/独立Plan EmptyExisting/安装计划create和reuse都不跳过规则；不要只在最后insert防新建而漏purePlan/reuse。
- `internal/store/playbook_tx.go` 的createInstalledSourceOn保最后同validator防御（不碰引用查询/配置判等/回执字段）。回滚/预先验证确保整包0写，已插cap/源/receipt/token计数不能残留。
- currentStore PB dryRun沿该Render/Plan验证一致，不能dryRun给ready然后实际Install才失败。外部引用读取不借此新增一般GET旧row限制；不迁已有数据库/不做数据清理。

## 顺序与所有权

B tests-only writer先安全保存当前源/日志/白盒/78映射，不让它越界改生产；若仍live，待其收齐/停止后由同manager下一唯一**Source-name small product writer**接。父允许上述4个生产路径（新name.go、source.go、render.go、playbook_tx.go），另event.go仅existing-sentinel同指针别名/import，以及相应sourcespec name测试、store source/playbook/v4test精确规则测试、团队报告。

不新manager、不改C3鉴权/预算/结果/时间/Node/protocol/前端，不重新实现全Source schema。若现包已有能放此validator的确切common位置可复用，仍只此规则，禁止为了复用扩大其它行为。

## 必测与机械gate

- 共validator：空/空白/trim后的route拒；已有合法名字与CAP旧四名不被新禁；不默默rename。
- 同有效物化Source名route：directCreate、pureRender、independentPlan、stored dryRun/actualInstall、已有同名reuse计划均按现规则拒。先真实RED后GREEN，不新增无根据的旧名称禁令。
- 整包包含正常能力+该错误source：所有cap/source/edge/receipt/相关真实persistent state前后0变。保原B红例与脱敏安全（报告不另复制rawConfig凭证）。
- 合法Source webhook/cron/manual与空targets允许/no_target、配置/targets真冲突矩阵、installedFrom/回执/原输入不改变。
- 原完整store/auth/push三gate不減；因共享sourcespec/playbook生产修改，补其go test/vet目标。原白盒/78逐案mapping如未完照pending，完整G1fresh门槛后才G2/G3/G4/final全Go-contract-quick。

这是已证同入口规则漏洞修复，不是新v3兼容名单或产品架构。尚未通过实际gate/fresh前不声明B或T1完成。
