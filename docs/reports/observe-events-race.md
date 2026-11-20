<!-- snapshot-doc: 2026-11-21 #493 取证与交付时的定格数字（真 Chrome、本机串行 e2e 输出） -->
# observe-events 偶发 `AssertionError /Ingested/ Input: ''` —— 断言在「查询中」窗口里读 `#ev-list`（#493）

> 分支 `pi-subagent/hw-nj/anotify-obs-events-2b0d0434` · 基线 `origin/main` `da9f7e44`
> 现场 CI run `44727996065`（PR #479 `c26a2c61`）
> 签名 `f3-observability.mjs:82:12` · `The input did not match the regular expression /Ingested/. Input: ''`
> 涉及面 **只有** `scripts/e2e/fixtures/f3/f3-observability.mjs`（不改产品、不碰 #479 的业务改动、
> 不放宽断言、不登记 `known-env-failures.json`）

**一句话**：不是事件丢了，也不是 #479 的业务改动；是**断言读得太早**。events 页每一轮查询都
「先清屏再画」——`reset()` 把 `#ev-list` 换成骨架行（骨架行没有文本），等 `/v1/console/events`
回来才 `render()`。断言在这个「查询中」的窗口里读 `innerText`，拿到 `''`，于是红成
`did not match /Ingested/`。最后一轮查询是**点击副作用**：点了 `#ev-list` 里的 `summary`，
焦点离开文本框，浏览器补一个**原生 `change`**，#479 把筛选改成「change 即查询」后，页面于是
又发了一轮查询 —— 断言正好读在这一轮里。

---

## 1. 现场（可复核的原文）

```
frontend / observe-events — AssertionError: The input did not match the regular expression /Ingested/.
Input: ''
  at observability (scripts/e2e/fixtures/f3/f3-observability.mjs:82:12)
```

判定层把 assertion 归为「不重试」，所以这条抖一次就是硬红。同类失败今天已经修过一处
（fetch 包装器先写 `httpStatus` 再解析 body、`waitPost` 只等状态码 ⇒ 读到半条记录）——**同源：
读的一方没有等「那份事实已经落地」**，只是这次落在 DOM 上。

## 2. `input` 到底是什么（先答这个问题）

| 问题 | 答案 |
| --- | --- |
| `input` 从哪来 | `await page.locator('#ev-list').innerText()` —— **DOM 文本**（不是 fetch 响应、不是 `innerHTML`）。`#ev-list` 是事件行容器 |
| 期望值 `/Ingested/` 从哪来 | `opened.spec.reason`。`calls()` 是 `window.__f3Calls`（fetch 包装器的镜像，**fetch 之前就 push**）；`find()` 取的是本 case **第一次** `/console/events`（首屏 `namespace=default&limit=20`，4 条）响应里的 `items[0].spec.reason` |
| 所以失败只有一种形态 | **DOM 此刻是空的**，而 API 事实（`Ingested`）一直在。`''` 不是「数据没产生」，是「还没画上去」 |

`#ev-list` 出现的空串只有两种来源，都指向同一件事：

1. **查询中**：`reset()` → `list.replaceChildren(skeletonRows(4))`（`web-src/pages/events.html`），
   骨架行是 `<span class="skeleton">`，没有文本 ⇒ `innerText === ''`；
2. 出错清屏：`load()` 的 `catch` 里 `if (!rows.size) list.replaceChildren()`。

## 3. 缺的是哪种等待

断言前只有 **DOM 计数**等待（`article.length === 2`），**没有**「这一轮查询已画完」的等待。
而 `article` 计数不能当选好锚点：

- 计数满足只能说明「某一刻是 2 行」——上一轮的旧行在被顶掉之前也满足；
- 骨架窗口里计数是 **0**，读文本会拿到 `''`。

真正的前置条件是「当前这一轮已经渲染完」，对应页面上两个**真实**标记：
`#ev-list` 没有 `aria-busy`、且没有 `.ev-skel-row`。`scripts/e2e/**` 里已有同形状的锚点
（`f3-overview-all.mjs` 的 `settle()`），本报告复用它，不发明新机制。

## 4. 机制（实证，不是推断）

在 `e2e/frontend` 用例的页面上下文里挂监听（**诊断用，不进交付**）：

```js
await page.evaluate(() => {
  window.__f3Diag = { trusted: 0, untrusted: 0, atClick: -1, afterClick: -1 };
  for (const id of ['ev-trace', 'ev-source']) {
    document.getElementById(id).addEventListener('change', (e) => {
      window.__f3Diag[e.isTrusted ? 'trusted' : 'untrusted']++;
    });
  }
});
// …在点 #ev-list summary 之前记 __f3Calls.length，点之后 250ms 再取一次
```

`#479` 树（`c26a2c61`）连跑 3 次，**每次输出完全一致**：

```
DIAG {"trusted":4,"untrusted":4,"atClick":16,"afterClick":17,"skel":0,"arts":2,"busy":null,"focused":""}
```

- `trusted: 4` = 浏览器自己补的 4 个**可信** `change`（用例里 `dispatchEvent` 的 4 个是 `untrusted`
  的）。`fill()` 只发 `input`，`change` 是**失焦时补的**，所以每次跑都多出这一轮查询；
- `atClick: 16 → afterClick: 17` = 点 `summary` **之后**又发了一轮 `/console/events`。

也就是说：这条竞态**每轮都存在**，只是骨架窗口只有几毫秒 —— 本机通常够快（`skel:0, arts:2`），
CI 慢/抖动时就读在窗口里。这不是「偶发行为」，是「偶发命中」。

失败现场的 `report.json`（运行时目录 `…/fixtures/f3/screenshots/f3/runs/<runId>/`（产物，不入库））也能对上：
失败那次在 `continue` 400 之后**还有一发** `namespace=prod&limit=2`，正是这轮点击副作用。

## 5. 复现数字

全部**串行**跑 `node scripts/e2e/suites/frontend.mjs --case observe-events`（同一 case 并发跑会撞
harness 的端口段与 epoch 指纹，见 §8 的教训，数字不可用）：

| 树 | 自然跑 | 结果 |
| --- | --- | --- |
| `origin/main` `da9f7e44`（无 `change 即查询`） | 30 | **0 / 30 失败** |
| #479 树 `c26a2c61`（含 `change 即查询`） | 30 | **1 / 30 失败**（`82:12` + `Input: ''`，与 CI 同签名） |
| 本改动（`settledList()`）· main 树 | 40 | **0 / 40 失败** |
| 本改动（`settledList()`）· #479 树 | 40 | **0 / 40 失败** |

**`origin/main` 上不可复现** —— 但这不是「main 没有问题」，是 main 的事件页还没有
「change 即查询」：`web-src/pages/events.html` 在 main 上只有 `submit` 监听，点击不会带出
第 17 发查询，所以那个窗口在 main 上不存在。**#479 一旦合入，这条竞态就变成常态**（每轮都有
一发点击副作用查询），所以修法进 main 是 #479 的前置条件，而不是 #479 的业务改动。

## 6. 修法

`scripts/e2e/fixtures/f3/f3-observability.mjs`：读面板内容前等真实条件。

```js
/* 事件面板的「查询已落下」锚点 —— 用真实条件而不是时长。 */
const settledList = () => page.waitForFunction(() => {
  const list = document.getElementById('ev-list');
  return !!list && !list.hasAttribute('aria-busy') && !list.querySelector('.ev-skel-row');
}, null, { timeout: 15000 });
```

调用点：点完 `#ev-list details summary` 之后、读 `innerText` / `.ev-chip` / `textContent` 之前。

**不是这些**：不是加长 sleep（`waitForTimeout` 在慢机器上仍然会输）；不是放宽正则；不是把 `''`
也算过；不是 `skip`；**没有**往 `known-env-failures.json` 登记任何东西。
超时仍然让 case **红**（`waitForFunction` 15s 抛错），失败信息里带真实条件，不静默。

顺带修掉一个**假绿**风险：`emptyChips` 守卫原本读 `#ev-list .ev-chip`，在骨架窗口里
`0 个空 chip` 会**空过**（列表根本没画）。等 settled 之后这条守卫才真的在验东西。

## 7. 变异实验（去掉等待必须能红）

在 #479 树上把「确定性等待」换成另两种写法，并**注入 250ms 响应延迟**（把 CI 的慢放大成
确定性窗口，`page.route('**/v1/console/events*')`）——三种变体只差这一处：

| 变体 | 250ms 延迟下的结果 |
| --- | --- |
| `settledList()`（本改动） | ✅ PASS（3/3） |
| 变异 M1：完全不等（删掉等待） | ❌ FAIL（3/3）`did not match /Ingested/` + `Input: ''` |
| 变异 M2：换成固定 sleep（`waitForTimeout(0)`） | ❌ FAIL（3/3）同签名 |

M1/M2 的失败原文与 CI 现场**逐字一致**（`Input:` 后面是空行 + `''`）。这就是「断言真正需要的
条件」与「固定时长」的差别：延迟是 250ms 还是 3ms 都不影响 `settledList()`，但任何固定 sleep
都只是赌。（注入的那几行把断言从 `82:12` 推到了 `97:12`，是同一处断言 —— 行号差异来自临时注入，
不是另一个错误。）

## 8. 本地验证

- `--case observe-events` 串行 40 次：main 树 0 失败、#479 树 0 失败（§5）；
- `make ci` → **exit 0**（含 gofmt / vet / 单测 / 前端产物 / 各类守卫 / `harness_selftest`）；
- `make e2e-affected` → **exit 0**，选中的 5 套全绿（断言 **215 通过 / 0 失败**，总耗时 253.6s，
  套件级与 case 级重试均为 0）：
  `frontend 43/0` · `i18n 33/0` · `i18n_coverage 44/0` · `ui_shift 87/0` · `ux_regression_19_23 8/0`
  （清单内 0 / 清单外 0 —— 失败数为 0，`known-env-failures.json` 一条都没命中）；
- `scripts/e2e/known-env-failures.json` **未改**（本报告与本次提交都没有为这条登记任何 entries）。
- 两种权威都过：除上面的 real 权威外，`E2E_MOCK_ONLY=1` 单跑一次 `✅ observe-events`（mock 预览
  走同一份 events 页与同一套骨架行逻辑，没有只修 real 分支）。

**一条实验教训**（写下来防止下次再踩）：同一个 case 用 `-P4` 并发跑**不是**有效的复现方法 ——
① harness 的端口段（`5900 + rand(400)`）是给「不同 case」并发用的，同一个 case 堆 8 个进程会
出现 `服务启动失败（试 5 个端口均不可用）`（实测 60 跑里 25 跑死在这）；
② 树里改任何一个 `f3-*.mjs` 都会让**在跑**的 fragment 报 `Source code changed within run`
（本次实验只是补了一下注释里的 issue 号就撞上了）。所以上面的数字全是串行的
（= CI 的 `frontend / <case>` 单 case job 形态）。

## 9. 产品侧结论：没有产品 bug

- `spec.reason = Ingested` 一直在（`/v1/console/events` 首屏 4 条都带），事件没有丢；
- 页面的「先清屏再画」本身不是 bug：查询期间的骨架行是 ST-8 明确的加载态语义；
- 「失焦补 `change`」是浏览器行为，`fill()` 之后确实算「值变了」，页面按 change 查询也是
  #479 想要的接线。**要修的是读的一方**。

## 10. 遗留 / 交接

1. **#479 需要 rebase 后保留这个等待**。它把 `events` 分支的这几行重写过，冲突要按语义合：
   在那个版本里，读 `#ev-list` 之前（`await page.locator('#ev-list details').first().locator('summary').click();`
   之后）加一行 `await settledList();`，并把 `settledList()` 一起带进来。**不保留 = 这条偶发红还会在。**
   另一种更彻底的做法（可选）：在 `fill()` 之后显式 `blur()`，把那次原生 `change` 变成确定会发生的
   那一轮查询，后续点击就不再产生「惊喜查询」。
2. `#484` 早期清单里的「同一行」无法回溯确认：`82:12` 这个行号只可能来自含 #479 改动的树
   （`#484` 最终树里这条断言在 65 行、且没有 `change 即查询` 的改动）。所以「#484 rebase 后跑绿」
   **不能**当作这条竞态修好了的证据 —— 它在 #479 树上仍有 1/30 的自然命中率（§5）。
3. 同类形状的其他断言已经逐个核过（不盲推）：`events_paging` 只读计数/uid（`waitForList` 也是
   计数锚点，但它根本不读面板文本，吃不到这个签名）；`trace` 分支读 `#trace-events.innerText`
   之前必经 `goto()`，而 `goto` 会等到页面上没有 `aria-busy` 才返回（真实锚点）。所以本次只动
   `observability` 的 events 分支。规则记下来：**读面板文本前先等「这一轮画完」，不要拿计数当锚点。**

## 附：复现/变异用的临时注入（不进交付）

```js
// 1) 把 CI 的慢放大成确定性窗口（装完 route 后，点击副作用那发查询会慢 250ms）
if (process.env.F3_PROOF_DELAY_MS) {
  const ms = Number(process.env.F3_PROOF_DELAY_MS);
  await page.route('**/v1/console/events*', async (route) => {
    await new Promise((r) => setTimeout(r, ms));
    await route.continue();
  });
}
// 2) 三种变体只差这一处
const settleEvents = async () => {
  if (process.env.F3_PROOF_MUTANT === 'none') return;                      // M1：不等
  if (process.env.F3_PROOF_MUTANT === 'sleep') return page.waitForTimeout(0); // M2：固定 sleep
  await settledList();                                                     // 本改动
};
await settleEvents();
assert.match(await page.locator('#ev-list').innerText(), new RegExp(opened.spec.reason));
```
