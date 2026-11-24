<!-- snapshot-doc: 2026-11-24 #563 交付时的定格数字（命中率与门禁输出都是这次运行的实录；复跑以新输出为准，不要把本文件的数字当现值） -->

# ux21 `/auth/me` 身份假红：那次 200 的 body 根本没解析（#563）

结论先说：**失败的第二次 `/auth/me` 不是服务端、也不是 mock 给的「200 空 body」，
它是驱动记录器里一条「状态码先落、body 还没解析」的半条记录。** 断言用
`httpStatus` 当「已落定」，于是把这条半成品读成了「另一份身份」。
`internal/server` 与 `web/mock.js` 都排除了；#483 已经把正确判据（`settled`）加进驱动，
ux21 是漏改的那一处。

## 1. 现象（CI 现场，PR #562）

```
ux_regression_19_23 / ux21-authme-identity
#21 every /auth/me response is the same identity, no divergent/fake user:
  ["usr_mock_demo|demo","|"]
  [{"hs":200,"id":"usr_mock_demo","user":"demo"},{"hs":200}]
AssertionError: 2 !== 1
```

两次 `/v1/auth/me` 都是 200，第二次没有 `id`/`username`，重跑就绿。同一 PR 其余套件全绿、
`make ci` 绿 —— 典型的「采样时机」而不是「产品行为」。

## 2. 定性：先列假设，逐条排除

| # | 假设 | 结论 | 证据 |
| --- | --- | --- | --- |
| H1 | `?u=zheng` 的「假用户」路径让某次 `/auth/me` 用了假身份 | **排除** | `?u=zheng` 只是 DOM 占位：`web-src/pages/index.html:1177` 读 `qs.get("u")` → `:1201` 传给 `mountLayout` 当侧栏初始文本，真实身份回来后由 `web/partials.js:1847-1854` 覆盖。全链路不发请求、不进记录器、不碰 mock 分支。 |
| H2 | mock 会顶掉真响应 / mock 只覆盖了部分请求 | **排除** | mock 是 `window.fetch` 的 monkey-patch（`web/mock.js:5447`），对 `/v1/auth/me` 只有两种产出：`hasSession() ? json(ME) : anonymous()`（`web/mock.js:2998`）；`json()` 恒带 `Content-Type: application/json` 且 body 是完整 `ME`（`web/mock.js:93-100`），`anonymous()` 是 **401 text/plain**（`web/mock.js:2979-2981`）。未实现的路由是 **501 + console.warn**（`web/mock.js:5504`），mock 权威下凡漏到网络层的 `/v1/*` 也是 **501**（`scripts/e2e/fixtures/f3/f3-driver.mjs:633-640`）。**没有任何一条路径能产出「200 且没有 body」。** |
| H3 | 服务端 200 但缺字段 | **排除** | `internal/server/auth_handlers.go:201-218`：`uid==""` → 401，`GetUser` 失败 → 404，成功必写 `id/username/displayName/role/disabled/createdAt`。ux21 跑在 explicit-mock 权威下，压根没有 Go 服务进程。 |
| H4 | 两次响应真的是两个不同身份（有人被 mock 成假用户） | **排除** | 失败快照里第二条记录的**字段集**就没有 `result`：`keys` 停在 `…+httpStatus`，没有 `result`/`response`/`settled`。它不是「另一个身份」，它是「这次调用的 body 还没解析」。见 §3。 |
| H5 | 记录层的半条记录被当成已落定 | **确认** | 见 §3/§4。驱动是两段式写入（`f3-driver.mjs:816` 落 `httpStatus`、`:823` 才落 `result`、`:836` 才置 `settled`），ux21 的 `pendingOf` 只看前两者之一。这正是驱动里 #483 注释写下的同一形态（`f3-yaml-create.mjs:91-96`、`f3-source-poll.mjs:49` 早就改用 `settled`）。 |
| H6 | `window.Anotify` 被赋值两次导致 `fetch` 双包、同一次请求记两条 | **排除** | 双包只出现在 `web-src/pages/cli-auth.html:1193`、`web-src/pages/passkey-enroll.html:1351`；index 页没有 `window.Anotify = window.Anotify || {}` 这种写法。而且双包会让两条记录都拿到 `result`，与现场相反。 |

## 3. 确认的机制

`f3-driver.mjs` 的 `window.fetch` 包装是**两段式**写一条记录：

```js
__f3Calls.push(call);            // 802 fetch 发起前就占一行（只有 url/method/body/keys）
…
call.httpStatus=response.status;            // 816 响应头到了 → 先落状态码
const text=await response.clone().text();   // 822 读 body 是**跨任务**的（实测：至少一次 task）
call.bodyBytes=text.length;
call.result=JSON.parse(text);               // 823 body 解析完
…
call.settled=true;                          // 836 只有到这里才是「完整记录」
```

`page.evaluate` 采样是一个任务，所以它**能**落在 816 和 823 之间：这时记录是
`{url, method, …, httpStatus: 200}` —— 有状态码、没有 body、没有 `error`。
ux21 旧的就绪判据是

```js
const pendingOf = calls => calls.filter(x => x.httpStatus === undefined && x.error === undefined);
```

半条记录不算 pending → 循环 break → `identities` 里多出 `"|"` → 断言红。
页面上两处 `/auth/me`（layout 的 `loadMe` @ `web/partials.js:1847`、页面控制器的
`loadMe` @ `web-src/pages/index.html:1478`）**并发**，第二条被采样到半条，就是 CI 的
`[{"hs":200,…,"user":"demo"},{"hs":200}]`。

页面内的采样器（每次任务采样一次 `__f3Calls`）实测到窗口宽度：新建浏览器的第一个
document 里，第一条 `/auth/me` 的 `httpStatus → result` 间隔 **82ms**，第二条 **33ms**。
窗口不是微秒级，所以「偶发」但真会发生。

## 4. 复现的驱动条件与命中率

本机（8 核）顺序跑根本撞不上；CI 的形状是 **2 核 runner × 4 分片 × 每分片 2 并发**。
用同样的形状复现（`taskset -c 0,1` + 8 个并发 case + CI 自己的超时 env
`F3_NAV_TIMEOUT_MS=20000 F3_DEFAULT_TIMEOUT_MS=15000`）：

| 条件 | 次数 | 命中 | 日志 |
| --- | --- | --- | --- |
| 修前 · 单跑 `--case ux21-authme-identity` 顺序 | 20 | 0 | — |
| 修前 · 同上 6 并发 | 60 | 0 | — |
| 修前 · `ux_regression_19_23` 整套 2 并发 | 24 | 0 | — |
| 修前 · 2 核 pin + 2 并发（±2 CPU hog） | 20 | 0 | — |
| **修前 · CI 形状（2 核 pin + 8 并发 + CI 超时）** | **40** | **2（第 13、39 次）** | §7 rt-fail-1/2 |
| 修前 · 同形状 + 只把断言消息加字段（定性用） | 40 | 1（第 9 次） | §7 rt-diag |
| **修后 · 与修前完全同形状** | **40** | **0** | — |

「顺序/中等并发 104 次 0 命中、CI 形状 40 次 2 命中」正好解释了这条 issue 的性格：
不是随机 flake，是**采样相位**的 flake——本机够快时 50ms 网格总是落在窗口外面。

## 5. 修复

1. `scripts/e2e/fixtures/f3/f3-driver.mjs`：记录 `contentType`（始终）与 `bodyBytes`
   （解析时）。现在「200 但没有 body」和「200 且 body 缺字段」在日志里分得开；
   `report.requests` 归档也带上这两个字段。
2. `scripts/e2e/fixtures/f3/f3-ui-cases.mjs`（ux21）：
   - 就绪条件改成 `meCalls.every(x => x.settled === true)` —— 等的是「状态码 + 解析后的 body」，
     不是「状态码」。半条记录不算 response。
   - 断言改成**逐条列事实**，不再先拼 `id|username` 再比 `Set.size`：
     每条 `/auth/me` 必须 `id && username` 都在，且所有调用必须一致；
   - 失败消息里每条调用一行：`call: "2/2"`、`url`（含 query）、`hs`、`contentType`、
     `bodyBytes`、`hasBody`、`err`、`settled`、`id`/`username`。
3. 注释行随之下调 3 行，`node scripts/check-comment-density.mjs --update` 把目录基线
   `1479 → 1476`、总量 `43295 → 43292` 一并收紧（棘轮只降不升）。

修后同一现场会怎么说（用真实半条记录构造，见 §7 rt-message）：

```
#21 every recorded /auth/me call settled (httpStatus + parsed body; a half record is not a response):
[{"call":"1/2","url":"/v1/auth/me","hs":200,"contentType":"application/json; charset=utf-8",
  "bodyBytes":116,"hasBody":true,"settled":true,"id":"usr_mock_demo","username":"demo","displayName":"demo"},
 {"call":"2/2","url":"/v1/auth/me","hs":200,"contentType":"application/json; charset=utf-8",
  "bodyBytes":116,"hasBody":false,"settled":false}]
```

一句话就能说出「哪一次、为什么」：第 2/2 次调用 `hs=200` 但 `hasBody=false / settled=false`。

## 6. 没做的两件事（刻意不做，不是漏）

1. **不重试、不 skip、不抬超时、不写 known-env**：修的是「没等对」，不是「等得不够久」。
   超时时间、重试语义、`scripts/e2e/known-env-failures.json`（仍是 `entries: []`）一个字没动。
2. **没有给前端 `loadMe` 加「同 tick 复用」去重**：index 的两处 `/auth/me` 各有 DOM 归属
   （layout 填侧栏/头像/超管菜单，页面控制器填 greeting），是设计而非缺陷。
   (a) 去重**并不能**消掉这条 flake —— 只剩一次调用时，那条半记录照样会被读成空身份；
   (b) 加 promise 缓存要连带处理 login/logout 失效，会改动 22 个页面的 `mountLayout` 行为。
   这条 issue 的根因在记录器的就绪判据，不在请求数，所以这一轮只改判据（问题 3 的第二个分支
   在定性后被排除：页面控制器的第二次调用不是根因）。

## 7. 原始日志

<a id="rt-fail-1"></a>
**rt-fail-1（修前，CI 形状第 13 次）** 与 CI 现场逐字相同：

```
[F3 ux21-authme-identity] authority=explicit browser mock fixture, NOT T1/T3 integration
  ❌ ux21-authme-identity — AssertionError [ERR_ASSERTION]: #21 every /auth/me response is the same identity, no divergent/fake user: ["usr_mock_demo|demo","|"] [{"hs":200,"id":"usr_mock_demo","user":"demo"},{"hs":200}]

2 !== 1
```

<a id="rt-fail-2"></a>
**rt-fail-2（修前，同形状第 39 次）** 同上，一字不差。

<a id="rt-diag"></a>
**rt-diag（修前，同形状第 9 次；只给断言消息加字段，判据未动）** —— 定性证据：
第二条记录 `settled:false`、`hasBody:false`、字段集停在 `…+httpStatus`：

```
  ❌ ux21-authme-identity — AssertionError [ERR_ASSERTION]: #21 every /auth/me response is the same identity, no divergent/fake user: ["usr_mock_demo|demo","|"] [{"hs":200,"settled":true,"hasBody":true,"keys":"url+method+body+keys+configKeys+httpStatus+result+response+settled","id":"usr_mock_demo","user":"demo"},{"hs":200,"settled":false,"hasBody":false,"keys":"url+method+body+keys+configKeys+httpStatus"}]

2 !== 1
```

<a id="rt-message"></a>
**rt-message（修后，把一条记录改回半条，看新断言怎么说）**：

```
#21 every recorded /auth/me call settled (httpStatus + parsed body; a half record is not a response): [{"call":"1/2",…,"bodyBytes":116,"hasBody":true,"settled":true,"id":"usr_mock_demo","username":"demo","displayName":"demo"},{"call":"2/2","url":"/v1/auth/me","hs":200,"contentType":"application/json; charset=utf-8","bodyBytes":116,"hasBody":false,"settled":false}]
#21 every /auth/me call carries the full identity (id + username): [{"call":"1/2",…},{"call":"2/2",…,"hasBody":false,"settled":true}]
```

## 8. 验收

| 项 | 结果 |
| --- | --- |
| 修前命中（CI 形状 40 次） | **2 命中**（第 13、39 次），签名与 CI 逐字相同 |
| 修后同形状 40 次 | **0 命中** |
| `ux_regression_19_23` 单跑 20 次 | **20/20 绿**（每次 8 个 case 全过，串行，`F3_NAV_TIMEOUT_MS/F3_DEFAULT_TIMEOUT_MS` 与 CI 同值） |
| `make ci` | **exit 0** |
| `make e2e-affected` | **绿**：5/5 套件达标，断言 218 通过 / 0 失败，临时目录零残留 |

复跑命令（本机实测用的就是这两条）：

```
# ① 命中率（CI 形状：2 核 + 8 并发 + CI 自己的超时 env），--case 只是诊断开关
taskset -c 0,1 env F3_NAV_TIMEOUT_MS=20000 F3_DEFAULT_TIMEOUT_MS=15000 \
  E2E_MOCK_ONLY=1 node scripts/e2e/suites/ux_regression_19_23.mjs --case ux21-authme-identity
# ② 整套 20 次（每次 8 个 case，串行）
env F3_NAV_TIMEOUT_MS=20000 F3_DEFAULT_TIMEOUT_MS=15000 \
  node scripts/e2e/suites/ux_regression_19_23.mjs
```

②的等价门禁入口是 `./scripts/e2e/run_all.sh ux_regression_19_23`（多花三次 `go build`；
`make e2e-affected` 走的就是它）。上面的 20/20 是**整套**（8 case × 20 次）跑出来的，不是单 case。