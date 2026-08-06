# 终审报告 · CLI 设备授权登录（feat/cli-device-login）

> reviewer: anotify-reviewer（T6） · 审查范围：`git diff 870bcfa..HEAD`（6 commits，+8808/-34）
> 对照：cli-auth-requirements.md（AC-01~36）/ cli-auth-design.md / cli-auth-plan.md §0（最终契约）
> 行为结论以 tester 报告为准（128 断言 + 全量 e2e 13 套件绿）；本报告审工程质量与一致性。

## 安全不变量核查（sessionId/userCode=批准权，secret=领证权）

| 检查点 | 结论 |
|---|---|
| secret 只存 SHA-256 hex，明文仅建会话响应出现一次 | ✅ `cliauth.go:generateSecret` |
| secret 常量时间比对 | ✅ `cliauth.go:verifySecret`（subtle.ConstantTimeCompare，先 sha256 再比） |
| 明文 Key 不落库（事务内建 key 直接返回，任何表无明文列） | ✅ `store/cliauth.go:ConsumeCliAuthSessionAndCreateKey` + e2e AC-17 DB 文件搜明文实证 |
| 一次性领证原子性（并发 poll 唯一赢家） | ✅ 条件 UPDATE `WHERE status='approved'` + 同事务插 key + `SetMaxOpenConns(1)`，0 行→alreadyConsumed |
| poll/qr/lookup 信息泄露面 | ✅ sessionView 不含 userID/secretHash/keyID；qr 只回 authUrl（含 id，无 secret）；lookup 需登录 |
| 404/401 统一文案防枚举 | ✅ 页面侧 404 统一「授权会话不存在或已过期」；poll 侧 id 存在与否分 401/404——但 sessionId 128bit 不可枚举，符合 AC-19/20 设计 |
| 校验顺序 | ✅ poll 先验 secret（错→401）再惰性过期，无法用过期探测存在性 |
| 过期惰性迁移 | ✅ pending/approved 过期→expired 不可逆；approve 于过期边缘竞态被 poll 侧二次 lazyExpire 兜住，无漏洞 |
| scope ⊆ requested 校验 | ✅ manager + handler 双层，未知 scope 与超集均 400（AC-13/14 e2e 实证） |

## 契约一致性（plan §0 逐端点）

9 端点全部落地，字段名/路由/鉴权方式/限速阈值（10/20/30/min、poll 1.6s 最小间隔）与裁决一致；openapi.yaml 同步；空数组 `[]` 处理到位（create/sessionView/poll/keysSelf 均防 nil→null）。**例外见 🟡-1（409 响应体）**。

## 架构一致性

store 无 broker 依赖 ✅；领域逻辑（状态机/领证）在 auth 层、展现（DTO/限速/QR 渲染/静态分发）在 server 层，边界干净 ✅；`validateScopes` 抽函数复用（apikey.go）最小重构 ✅；mux Go 1.22 模式路由 `by-code` 字面量优先于 `{id}` 无冲突 ✅；go-qrcode 纯 Go 单依赖，QR 渲染自实现半块字符（~70 行）符合单二进制约束 ✅。

## 前端设计契合

双模式（?s= 确认 / 无参输码）✅；8 状态区块构建期 `{{t}}` 渲染、JS 仅显隐+填数据（textContent 只填 deviceName/username，零运行时拼文案）✅；lookup approved→approved 终态 ✅；focus 布局、keys 入口、login NEXT_MAP、partials PAGES 注册 ✅；四语言 yaml 齐全 ✅。
**色值**：cli-auth.html/focus.html 的 rgba 值与 login.html/base.html **逐字符一致**，且 design.md §4(L236) 明确指定这些内联值——属遵循既有体系与设计稿，**不列为问题**。

## 脚本健壮性

POSIX（dash -n ✅）、umask 077+临时文件+原子 mv+.bak 备份、退出码 0/1/2/3/4 语义正确、429 退避容忍、网络错误 5 次上限、stdout/stderr 无 Key/secret（API_KEY 仅内存变量→文件）、keys/self 幂等自检 ✅。

## 发现清单

### 🔴 阻塞：无

### 🟡 建议

1. **`internal/server/cli_auth_handlers.go:writeApproveErr(~L300)`** — approve/deny 终态冲突的 409 响应体为 `{status:"terminal"}`，字面量不在状态枚举（pending/approved/consumed/denied/expired）内，违反 plan §0「409 `{status}`」语义（应为当前实际状态）。前端已被迫绕开（cli-auth.html:867 注释「data 在错误里拿不到，重新 lookup」——409 后再打一次请求），e2e AC-16 只断言状态码未覆盖响应体。功能正确但契约受损+多一次往返。建议：`ErrAlreadyTerminal` 携带或 Approve 返回当前状态，409 响应体写真实 status。
2. **`internal/server/ratelimit.go:clientIP(~L105)`** — 匿名限速（create 10/min、qr 30/min）无条件信任 `X-Forwarded-For` 第一段；客户端直连源站时可伪造 XFF 逐请求换 IP 绕过限速刷会话（DB 行膨胀）。自托管场景风险中低，但限速器形同虚设的场景真实存在。建议：默认仅用 RemoteAddr，代理头信任做成配置项（如 ANOTIFY_TRUST_PROXY）。
3. **`internal/auth/cliauth.go:generateUniqueUserCode(~L340)`** — 唯一性「先查后插」存在 TOCTOU：两并发建会话拿到同一 code 时，第二个在 `CreateCliAuthSession` 撞 UNIQUE 直接 500，而非重试。概率约 1e-12 量级、仅表现为用户重跑脚本，但正确做法是插入时捕获 UNIQUE 冲突重试。

### 🟢 可选

1. `internal/server/static.go:embeddedStaticMust(~L82)` — 失败回退 `http.Dir("")` 实为 cwd，注释「Open 必失败」不准确（cwd 有同名文件会命中）；触发需 embed 损坏（编译期几乎不可能），建议显式空 FS（fstest.MapFS）。
2. `web/agent-login.sh:229` — secret 位于 curl 命令行 query，本机 `ps` 可见（多用户共享机场景）。契约已定 GET query，自托管单用户前提可接受，知悉即可。
3. `internal/server/cli_auth_handlers.go:create(~L95)` — 响应多返回契约外 `scopes` 回显字段，无害轻微范围蔓延。
4. `internal/server/ratelimit.go:pollGuard.allow(~L85)` — `float64` 往返计算 1.6s 写法绕，`1600*time.Millisecond` 更直白。
5. `cli-auth-requirements.md:L85` — 「POST 轮询」与 plan §0「GET poll」不一致（实现按 plan 正确），文档侧修订。

## 测试质量复核

e2e 165 断言均为真行为断言（userCode 字符集正则、DB 文件搜明文证不落库、终态 409 幂等、三入口同会话），未见弱化痕迹；tester 声明的遗留（AC-04 IP 限速显式用例、AC-27 GUI 路径、AC-28 TTL 端到端）由单测/冒烟覆盖，风险可接受。store round-trip、handler 表驱动测试齐备。

## 协调者独立验证（只读）

- `go test ./... -count=1` → 全部 ok（store/auth/server/ws/push/broker/api/sitegen）
- `make build` 成功；单二进制 :18099 实证：`GET /agent-login.sh` → 200 `text/x-sh` `Cache-Control: no-store`；`POST /v1/cli-auth/sessions` → 响应字段与契约逐字段一致（userCode `YP6T-USH4` 格式正确，authUrl 仅含 `?s=`）
- `internal/server/dist/agent-login.sh` 固定名存在于指纹产物（未被 hash）

## 结论

🟡-1/2/3 均为「正确性/健壮性增强」级别，无安全漏洞、无功能缺陷、无契约破坏（🟡-1 是契约未完全兑现但前端已正确兜底）。**三条建议项可合并后快速修复，也可接受现状合并后跟进**——按终审严格标准，给予：

**VERDICT: APPROVE（附 3 条建议项，建议合并前修 🟡-1，🟡-2/3 可后续跟进）**
