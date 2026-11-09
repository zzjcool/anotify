# N2 TESTS（N2A checkpoint · 需求 → 具名测试映射）

> 任务：V4-ANODE-N2A。所有测试都是**真行为**：真解析、真文件、真 `httptest` HTTP 服务、
> 真 `coder/websocket` 连接；没有 skip / stub / 弱断言。文件为 N2A checkpoint，
> 未覆盖项属于 N2B（见 `N2-INTERFACES.md §9`）。
>
> 测试总数（A 阶段）：**69 个顶层 Test（含子测试共 100 个入口）**，全部通过。

## 1. 固定机械验证（N2 三条，实跑）

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
  go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1
# ok  internal/protocol
# ok  internal/artifact
# ok  cmd/anode/internal/applied
# ok  cmd/anode/internal/capability
# ok  cmd/anode/internal/config
# ok  cmd/anode/internal/hostenv
# ok  cmd/anode/internal/hub
# ok  cmd/anode/internal/listen
# ok  cmd/anode/internal/node        （exit 0）

GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
  go vet ./internal/protocol/... ./cmd/anode/...
# （无输出，exit 0）

git diff --check
# （无输出，exit 0）

gofmt -l cmd/anode/   # 空
```

## 2. 需求 → 测试

### 2.1 strict `anode.yaml`（config）

| 需求 | 测试 |
|---|---|
| name/server/token 必填、逐项列出 | `TestRequiredIdentityFields` |
| name 正则、server ws/wss、URL 不带凭据 | `TestNameAndServerValidation` |
| 未知字段（顶层 + 嵌套）失败 | `TestStrictUnknownFields` |
| 多 YAML 文档失败 | `TestMultiDocumentRejected` |
| namespaces 缺省 `[default]` / 显式空失败 / 重复失败 / 非法元素 | `TestNamespacesDefaultExplicitEmptyAndDuplicate` |
| vars 名大写正则、值非空 / ≤4096 / 无 NUL | `TestVarsValidation` |
| limits `total` **未写 vs 显式 0**（0/-1 失败） | `TestLimitsUnsetVsExplicitZero` |
| perCapability 键（能力名 / `*`）、值 ≥1 | `TestLimitsUnsetVsExplicitZero` |
| envPassthrough ≤32 / 去重 / 非法名 / PATH / HOME / ANOTIFY_ | `TestEnvPassthroughValidation` |
| listen 缺省 `tcp://127.0.0.1:0` / tcp / unix / 非法 scheme | `TestListenValidation` |
| 解析成功字段值 | `TestParseValidConfig` |

### 2.2 合并 / hash / 权限 / applied

| 需求 | 测试 |
|---|---|
| vars local > node 逐键 | `TestMergeVarsLocalOverNode` |
| limits 递归逐叶（total 未写继承、perCapability 覆盖/继承/`*`） | `TestMergeLimitsLeaf` |
| MachineConfigHash 唯一实现（protocol golden 交叉验证） | `TestMachineHashUsesProtocolImplementation` |
| ns = 申报 ∩ 授权；本地不能扩张；`*` 语义 | `TestEffectiveNamespacesIntersection`、`TestAllowsNamespace` |
| welcome 节点默认值也是严格校验 | `TestValidateNodeDefaults`、`TestApplyRejectsInvalidNodeDefaults` |
| applied 往返 + 0600 + 无临时残留 | `TestSaveAndLoadRoundTrip`、`TestSaveAtomicMode0600NoTempLeftovers` |
| 冷启动无文件 = Present false（不造旧在线事实） | `TestLoadColdStart` |
| applied strict：未知字段 / 坏 hash / 符号链接 | `TestLoadStrictUnknownField`、`TestLoadHashMismatch`、`TestLoadRejectsSymlink` |
| 覆盖符号链接安全（不写穿目标） | `TestSaveReplacesSymlinkSafely`、`TestSaveRejectsDirectoryPath` |
| **ns 默认值不落 applied**（顶层键只有 revision/hash/vars/limits） | `TestNsDefaultsNotInAppliedFile` |
| 重启加载 applied、本地配置变了重新 merge | `TestWelcomeIdempotentAndRestartRemergesLocal` |
| 坏 hash applied 重启不被信任、welcome 覆盖恢复 | `TestCorruptAppliedIsNotTrustedOnRestart` |
| 写盘成功才切生效值/才回报 | `TestWelcomeApplyWritesAppliedThenFrame`、`TestApplySaveFailureKeepsState` |

### 2.3 安全产物 GET + cache + 部署回执

| 需求 | 测试 |
|---|---|
| Bearer GET `/v1/node/capabilities/{digest}`、路径/认证头 | `TestFetchSuccess` |
| 400 / 401 / 403 / 404 / 5xx 封闭错误码 | `TestFetchStatusErrors`（子测试 5 个） |
| 响应 digest 与请求不符 | `TestFetchDigestMismatch` |
| 响应 name/version 与 refs 不符 | `TestFetchRefMismatch` |
| 非法 spec（artifact.Validate） | `TestFetchInvalidSpec` |
| ns 未授权本地拒绝且**不发 GET** | `TestFetchNamespaceDeniedNoRequest` |
| 非法 refs 形状 | `TestFetchBadRef` |
| cache 原子 0600 / 目录 0700 / 读回校验 | `TestCacheStoreLoadRoundTrip` |
| digest 不一致拒绝写入 | `TestCacheStoreRejectsDigestMismatch` |
| 损坏内容拒绝（Load + Entries 隔离） | `TestCacheRejectsCorruptContent`、`TestCacheEntriesKeepsGoodSkipsCorrupt` |
| 符号链接条目目录 / 文件拒绝 | `TestCacheRejectsSymlinkEntryDir`、`TestCacheRejectsSymlinkFile` |
| 成功部署：缓存 + applied 回执同 refs | `TestDeploySuccessCachesAndAcks` |
| 失败部署：同 refs + failed + 一行原因 + 不缓存 | `TestDeployFailureAckSameRefs` |
| 未授权 ns 不触发 GET | `TestDeployUnauthorizedNamespaceNoGet` |
| 显式帧之外不动作（nil 帧安全失败） | `TestDeployNilFrame` |

### 2.4 hello / 探测 / 部署（node）

| 需求 | 测试 |
|---|---|
| 冷启动 hello：无 applied、无伪造槽位、共享 DTO 字段 | `TestColdStartHelloHasNoAppliedAndEmptyInstances` |
| token 只出现在 hello 身份字段；passthrough 名字/值不进 hello/applied/config.applied | `TestHelloDoesNotLeakPassthroughOrTokenToAppliedAndSnapshot` |
| welcome → 合并 → 落盘 → config.applied；槽位来自机器级 limits | `TestWelcomeApplyWritesAppliedThenFrame` |
| 权限交集在节点生效 | `TestWelcomeAuthorizationIntersection` |
| 命令探测用启动 PATH（与执行同源）| `TestHelloCapabilitiesProbeUsesStartupPATH` |
| 缓存实例 + 命令探测 + 事实共享 hello | 同上 + `TestColdStartHelloHasNoAppliedAndEmptyInstances` |
| 节点内部署入口真 GET/缓存/回执 | `TestDeployThroughNode` |

### 2.5 WS 客户端 / 配置流 / 重连

| 需求 | 测试 |
|---|---|
| 空 path 补 `/v1/node/stream`；显式 path 原样；非法 server 失败 | `TestStreamURLDefaultAndExplicitPath` |
| hello 首帧 + welcome → config.applied（真 WS） | `TestRunSendsHelloAndAppliesWelcome` |
| WS 握手 Bearer + hello.token；local total 未写发 0 | 同上 |
| 认证失败（401）= 终态，不重连/不退避 | `TestDialAuthFailureIsTerminal` |
| config.changed → config.get（带 appliedRevision/Hash）→ 新 welcome → 新 config.applied | `TestConfigChangedTriggersConfigGet` |
| 断线重连重发 hello（带 applied 状态） | `TestReconnectResendsHelloWithAppliedState` |
| 有界退避：1ms→2ms→4ms 封顶 | `TestReconnectBackoffBounded` |
| 部署帧 → 真 GET → 缓存 → capability.deployed（成功/403 失败） | `TestDeployFrameFetchesCachesAndAcks`（2 个子测试） |

### 2.6 启动环境 / 监听

| 需求 | 测试 |
|---|---|
| LookPath 用给定 PATH、返回绝对路径、执行位语义 | `TestLookPathUsesGivenPATH`、`TestLookPathNotFound` |
| PATH 启动快照不随后续 env 变化（执行同源） | `TestBaselineSnapshotStableAcrossEnvChange` |
| passthrough 只快照白名单名字的值、不重读环境 | `TestPassthroughSnapshotOnlyNames` |
| tcp `:0` 实际端点可连 | `TestOpenTCPActualEndpoint` |
| unix 监听 0600 / 关闭只删自己创建的 socket | `TestOpenUnixAndCloseRemovesSocket` |
| unix 已有普通文件/符号链接/目录拒绝且不删除 | `TestOpenUnixRefusesRegularFileAndSymlink` |
| 残留 socket 回收 / 活跃 socket 拒绝 | `TestOpenUnixReclaimsStaleSocket`、`TestOpenUnixRefusesActiveSocket` |

## 3. 未覆盖（N2B，明确不算 A 通过项）

- emit HTTP 网关与 `task.emit`/`task.emit.ack`（等父 E1 批准后的唯一 transport）。
- 执行 10 帧、真实 lease/进程组、cancel 回收、result/logs/ops。
- 任务环境展开（run.env/params/fieldPath）、workDir/HOME/files_escape 执行前校验、envPassthrough 实际注入。
- GC/保留、双流日志、`serve` 装配、`install`/`emit` 子命令（N3）。
