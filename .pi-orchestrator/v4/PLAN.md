# V4 全量实施计划

## 为什么这样分

三路 scout 已确认当前是 v3 数据模型，v4 的配置/授权帧/目录包/API信封大部分为新增；store→route→ws→server 为同一Go module的强依赖链。把这些模块分到独立隔离团队会在中途互相无法编译，容易逼出旧兼容层或虚假的绿灯。

初始采用两个团队；在 T1 的纯协议/产物已稳定后，**rev2 拆成三个隔离写团队**：T1 核心Go/store/调度/server/API/工具链，T2 静态前端/sitegen，T3 anode 本机执行+纯共享协议。T1内部仍按领域→运行→HTTP→集成串行推进；每个worktree同一时刻只有一个writer，侦察/review可并行。父会话负责协议快照接缝、整队复审、跨团队集成和最终门禁。

拆出 T3 的条件已经只读核实：当前 `internal/protocol` 仅stdlib，`internal/artifact` 仅stdlib+yaml，不依赖store/ws/route/SQLite；anode可独立编译测试而不恢复旧服务兼容。T3成为protocol唯一作者，先补具名的常量/hash/部署投递接缝并冻结给T1，不另起产品架构。

不是一次脚本替换；不承诺一小时完成，不缩减冻结核心范围。途中可恢复的阶段进度不等于最终交付。

## 已确认事实（不重复基线实测）

- artifact/store的Spec、DDL、task/node结构是全链路公共面；共享文件必须单写者。
- 旧source模板为{{path}}、edges带port/priority/mapping，v4必须移除，不做语法兜底。
- 旧assign直接授执行权、attempt派发递增；v4需dispatch→accept→grant。
- Node由namespace内对象变cluster对象，配置三层、机器applied与ns输入必须分开。
- 现有UI19个静态页，partials/mock/graphs/locale/CSS是真共享；source与生成HTML不能混改。
- 测试门禁已有make test/contract/fe/ci/e2e；旧fixture/契约checker绑定v3，必须重写语义断言，不skip。
- 已批准v4例外现已写入AGENTS/DEVELOPMENT：可重建baseline、拒绝旧库，不自动删除数据。

## 工作分解

| 编号 | 所属 | 交付 | 门禁 |
|---|---|---|---|
| G1 | T1 | artifact+纯共享协议、store基线/模型、版本/图投影、配置/授权事实、目录包安装/回执/GC | artifact/store/protocol包测试；新字段往返和行为测试 |
| G2 | T1 | route/ws/lease/source/trigger、消费T3协议的中枢授权/派发/结果/CAS与部署接线 | route/ws/lease包测试；旧attempt/取消/幂等专项 |
| N1 | T3 | 冻结纯protocol接缝（常量/hash/投递帧）+anode config/cache/WS client | protocol/artifact/anode独立包测试 |
| N2 | T3 | 本机grant执行/HTTPemit/lease deadline/取消/环境/files/产物GC | anode真实进程/httptest/WS模拟测试 |
| N3 | T3 | serve/install/emit三命令、安装脚本/服务/节点镜像/使用说明 | 全节点包测试、CLI自检、安装器离线自测 |
| G3 | T1 | /v1/namespaces路径、资源信封、派生status、CAS、人工deploy、OpenAPI/checkers | server/api测试、make contract、全Go编译 |
| G4 | T1 | devseed/E2E harness与非前端套件、构建/命名、保留域回归、测试迁移映射 | go test ./... -count=1、make contract、make e2e-quick |
| F1 | T2 | shared API helper/namespace scope、新信封mock、UI模型和图投影 | 新的JS单测/空数组错误CAS/ns隔离用例 |
| F2 | T2 | capability/source/playbook/node/config/task/event/trace/index等v4交互，4语言 | make fe、sitegen测试、前端单测、预览逐页验证 |
| F3 | T2 | 前端专属E2E及sitegen多spec文档输入，去旧端口/priority UI写路径 | 前端mock完整性、改页截图/console检查 |
| I1 | 父会话 | T1/T2fresh review→返修→按序集成 | critical/major清零，保留minor记录 |
| I2 | 父会话 | 最终真实v4服务+anode+前端交叉验证、指纹重建 | make ci、make build、全量make e2e、web_verify桌面/手机dark/light |

## 团队机械验收（父会话派发时固定）

T1：

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./... -count=1
make contract
make e2e-quick
```

T2：

```sh
make fe
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/sitegen/... -count=1
node --test scripts/v4-frontend.test.mjs
```

T3：

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/protocol/... ./cmd/anode/...
git diff --check
```

另要求T2起本地preview并逐个改动页面检查console/网络/滚动/溢出，保存dark/light截图；父会话仍独立验收。长期命令用async_run命名保存输出并收齐，禁止裸交互/无限socket等待。

## 明确暂不做

历史数据/旧API/旧WS兼容；中枢能力执行器；新的RBAC/多中枢/sandbox/凭证中心；K8s控制框架、labels/selectors/finalizers；replay API；安装自动改路由/部分conflict安装；自动重跑非幂等业务；删除用户目录；自动提交用户settings；推当前集成分支。

## 完成条件

不是文件改名完成，不是文档case通过，不是一个worker自报DONE。冻结D1～D6所有核心语义有真正实现与覆盖、保留功能无回归、两团队独立review通过、真实跨团队全量门禁通过，父会话才标完成。
