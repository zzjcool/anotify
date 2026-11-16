# #335 收口续派（实现 centurion）

> 读 `dispatch/COMMON.md` 全文（红线/基线/证据纪律）。你接手的是**被打断的既有工作**，不要重做已完成部分。
> issue：https://github.com/zzjcool/anotify/issues/335 ；PR：https://github.com/zzjcool/anotify/pull/354（base 已设为 `v4/integration`，**不要 merge**）。

## 工作树（唯一）

`/root/code/wt-335b`（已 bootstrap：node_modules 软链/安装齐、`.e2e-bin/{anotify,devseed,anode}` 存在）。**只在这里写**，不写 `/root/code/anotify` 主 checkout。不要新建 worktree。

```bash
cd /root/code/wt-335b
git fetch origin
git checkout -B issue/335-frontend origin/issue/335-frontend   # 当前 head 8645ff3
git rev-parse HEAD                                             # 必须 = 8645ff3
git merge --no-edit origin/v4/integration                      # 若已是最新则 no-op
```

base = `v4/integration` 66d926a（分支里已有 merge b8e4c18）；远程分支 `origin/issue/335-frontend` 是你的交付分支。

## 已确认基线（不要重做/重扫）

- **mock 已 35/35 EXIT=0**（12:21 `/tmp/w335b/mock-3.log`；父在 12:40 又跑一遍仍 0，见 `/root/tmp/335b/mock-now.log`）。
- `node scripts/v4-frontend.test.mjs` = **205 pass / 0 fail EXIT 0**（`/tmp/w335b/v4test-3.log`）。
- `make fe` / `node scripts/check-classes.mjs` = 0。
- **real frontend = 32/35**，三个失败（`/tmp/w335b/real-1.log`，真实二进制）：
  1. `source-auth-dedup` — 断言 `first.status===200` 实得 **401**。
  2. `capability-actions` — `#deploy-nodes input[type=checkbox]` 4.5s 超时（`scripts/e2e/fixtures/f3/f3-capability.mjs:273`）。
  3. `playbook-secret-history` — 真实回执 `unapplied[].declared/actual[0].params.blob` 是 `{inner:'F3_PRIVATE_NEW_OBJECT'}` 而非 `***`。

## 必须完成的修复（按此顺序）

### A. `source-auth-dedup` 真实分支对齐 #355（已合并 e897e01）

真实分支现在仍写着「v4 不做 HMAC 校验、只有 token」——**已过时**：#355 已让 source `config.secret` 参与 HMAC 验签（缺失/错误 → 401，不产生事件）。把 real 分支改成与 mock 分支同一组语义（保留全部原断言强度）：

- 建源时带 `secret` → 回执 `secretSet:true` 且响应不含明文；
- 未签名 → 401；错误签名 → 401；**均 0 写**（snapshot 前后相等）；
- 正确 `sha256=<hmac(raw bytes)>` → 200 + `tasks.length===1`；
- 同 `delivery id` 重投 → `duplicate:true` + 同 `eventId/traceId`；
- 投递 payload 解出 `params.message`；event `spec.deliveryId` 正确；
- PATCH 轮换 token/secret 后旧 hook/token → 404（按真实实现断言，读代码确认凭据轮换后旧 token 语义）。
- 签名必须对**原始字节**算（不要 `JSON.stringify` 后再算，测试里发的是固定 raw 串）。

### B. `capability-actions` 真实模式的人工部署节点候选

根因方向（先验证再改）：`#deploy-nodes` 的候选来自 **能力资源的 `status.nodes`**（`web/partials.js·nodesOf`，即“当前持有该能力的节点”只读事实），而 real fixture 里 `analyze-pr` 从未部署到任何节点 → 没有 checkbox。mock seed 有。**不要**把断言改成“没有节点也能过”，也不要改产品去列出未部署节点（除非契约明确要求并给出证据）。

正确修法（二选一，按真实契约与 fixture 事实决定，报告写理由）：
- 在 real bootstrap 里先把 `analyze-pr` 的一个版本真实部署到 `anode-1`（走真实 `POST /v1/namespaces/{ns}/capabilities/{name}/deploy` + 需要时 node config.applied），让 `status.nodes` 有候选；然后原断言（选节点 → 部署 v1 → 该节点 stale）继续成立；
- 或若契约规定候选应为 namespace 内所有可运行节点，则这是产品 bug（页面/模型），在 `web-src/pages/capabilities.html` + 必要的 `web/partials.js` 读模型内修产品并保留“无候选也能提交”的兜底；报告需给契约/接口证据。

不得用“延长超时/跳过/放宽断言”过关。修复后 mock 35/35 必须仍然绿（mock 有节点候选的情形不能坏）。

### C. `playbook-secret-history` —— **不是你的范围，等 #356**

真实产品缺口：`internal/playbook` 的 `unapplied declared/actual` 未对 writeOnly 值打码，已开独立 issue **#356**（`[v4][SEC] InstallReceipt 的 unapplied declared/actual 泄漏 writeOnly values`），由另一 centurion 在 `internal/playbook` 修。
你**不要改 `internal/**`**、不要改该断言、不要让 mock 迁就真实。先把它标 RED（证据在报告里），继续做 A/B/D；协调者会在 #356 合并后让你 `git merge origin/v4/integration` 并复跑 real。

### D. 五个 FE suite 的真实模式

#335 验收要求「新 source/artifact epoch 固定后五 FE suite 各完整执行一次」。目前只有 `frontend` 跑过 real。请对 `i18n`、`i18n_coverage`、`ui_shift`、`ux_regression_19_23` 各跑一次真实模式（默认非 `E2E_MOCK_ONLY`；若某 suite 的 shim 只支持 mock，给出文件:行号证据与真实可执行替代，不得假称跑过）。全部真实 exit + log 落 `/root/tmp/335/`。

## 验收命令（逐条真实跑，贴 exit；全部在 worktree 内）

```bash
export TMPDIR=/root/tmp/335; mkdir -p "$TMPDIR"
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto
make fe
node scripts/check-classes.mjs
node --test scripts/v4-frontend.test.mjs
E2E_MOCK_ONLY=1 node scripts/e2e/suites/frontend.mjs        # 两次，35/35
node scripts/e2e/suites/frontend.mjs                        # real；#356 未合前 34/35 可接受，须给唯一失败=secret-history
node scripts/e2e/suites/i18n.mjs                            # real 各一次
node scripts/e2e/suites/i18n_coverage.mjs
node scripts/e2e/suites/ui_shift.mjs
node scripts/e2e/suites/ux_regression_19_23.mjs
```

长命令 `set -o pipefail; timeout 900 ... 2>&1 | tee <log>; echo EXIT=${PIPESTATUS[0]}`。

## 交付

- commit（`type(scope): 中文`）+ `git push origin HEAD:issue/335-frontend`；**禁止** `gh pr create/merge`、禁止 push 到 `v4/integration`/`main`、禁止 rebase/force push。
- 报告格式按 COMMON §5，另给：mock/real 两轮结果、五 suite real 矩阵、#356 依赖状态、剩余失败逐条 file:line。
- 如果 A/B 完成后 real 只剩 secret-history：明确写「BLOCKED on #356」，把 HEAD push 上去，等协调者 continue 再合 #356 复跑。不要空转轮询。
