> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 2026-11-19 #453 修复落地时的定格数字（make ci / e2e 的真实输出），复算命令见 §8 -->

# 安全页「重新生成恢复码」：状态接口失败时渲染 8 枚 DEMO 假码（#453）

- 基线：`origin/main` = `3ec88fd2`
- 分支：`pi-subagent/hw-nj/anotify-453-f7a4fbdc`（独立 worktree）
- 结论：**真 bug，P0**。假数据路径已删除（不是加开关、不是改文案）；生成失败只报错，页面上不出现任何码；成功路径不变。

## 1. 现状（修复前代码原文）

两条分支共同构成事故：`loadRecoveryStatus()` 在状态接口失败时把 `recoveryDemo` 置真，`regenRecoveryCodes()` 看到它就直接本地造假码。

`web-src/pages/security.html`（修复前，`recoveryDemo` 分支）：

```js
  async function loadRecoveryStatus() {
    const data = await fetchApi(API + "/auth/recovery/status", {
      credentials: "same-origin",
    });
    if (data && typeof data.unusedCount === "number") {
      recoveryStatus = { unusedCount: data.unusedCount, hasCodes: !!data.hasCodes };
      recoveryDemo = false;
      if (!recoveryPlaintext) recoveryPlaintext = null;
    } else {
      recoveryDemo = true;
      recoveryStatus = { unusedCount: 0, hasCodes: false };
    }
    renderRecovery();
  }
```

```js
    try {
      if (recoveryDemo) {
        // 演示：本地假码
        recoveryPlaintext = Array.from({ length: 8 }, (_, i) => {
          const raw = ("DEMO" + (i + 1) + "CODEXXXXXXXX").slice(0, 16);
          return (
            raw.slice(0, 4) + "-" + raw.slice(4, 8) + "-" +
            raw.slice(8, 12) + "-" + raw.slice(12, 16)
          ).toUpperCase();
        });
        recoveryStatus = { unusedCount: 8, hasCodes: true };
        renderRecovery();
        toast(t("security.recovery.toast_regen", "已生成新恢复码，请立即保存"));
        return;                       // ← fetch 在 return 之后：一个请求都不发
      }
      const res = await fetch(API + "/auth/recovery/regenerate", { … });
```

后果链：状态接口 500/网络抖动 → 用户点「重新生成恢复码」→ **零请求** → 页面渲染 `DEMO1-COD-EXXX-XXXX` 形状的假码并提示「请立即保存」→ 用户存下 → 真丢设备时「用户名 + 恢复码」必然失败 → 锁号。

## 2. 根因

1. 「状态不可知」被当成「可以本地模拟」；正确语义是**不可知就不显示**。
2. 演示数据本应只由 `make mock` 的 `/mock.js` 在 fetch 层注入（#273 的既有约定：`keys.html`、`login.html`、同页 Passkey 分支都遵守）；恢复码这条是唯一漏网的**页面内假数据分支**。

## 3. 修法

- **删除** `recoveryDemo` 变量与整条演示分支：`DEMO` 码拼装、`unusedCount: 8` 伪造、假成功 toast 一并消失。
- 恢复码状态改三态：`recoveryStatus = null` 表示「状态不可知（接口失败）」。`renderRecovery()` 在该状态下清空网格、只显示「无法加载恢复码状态」，绝不填任何码。
- 生成失败统一走 catch：HTTP 非 2xx、网络错误、**或 200 但 `recoveryCodes` 为空**都只弹错误 toast「生成失败，请重试」；HTTP 细节只进 `console.error`，不再拼进用户文案。
- 四语言文案同步：`security.recovery.toast_regen_failed`（zh-CN / en / ja / es）都改成「生成失败，请重试」的对应表达，去掉 `{msg}` 占位。

```diff
@@ web-src/pages/security.html
-  let recoveryStatus = { unusedCount: 0, hasCodes: false };
-  let recoveryDemo = false;
+  // null = 状态不可知（接口失败）。这时页面上不出现任何码，也不冒充「暂无恢复码」
+  // —— #453：旧实现在这份状态下本地造了一组 DEMO 假码并提示「请立即保存」。
+  let recoveryStatus = null;
 
     copyAll.classList.add("hidden");
     grid.replaceChildren();
-    if (recoveryDemo) {
+    if (!recoveryStatus) {
       hint.textContent = t("security.recovery.toast_status_failed", "无法加载恢复码状态");
       return;
     }
@@ loadRecoveryStatus（失败分支）
-      recoveryDemo = true;
-      recoveryStatus = { unusedCount: 0, hasCodes: false };
+      // 状态不可知：保持 null，渲染「无法加载恢复码状态」，绝不填任何码。
+      recoveryStatus = null;
@@ regenRecoveryCodes
-      if (recoveryDemo) {
-        … 8 枚 DEMO 假码 + unusedCount: 8 + 成功 toast …
-        return;
-      }
+      // （整段删除）
       const codes = Array.isArray(data.recoveryCodes) ? data.recoveryCodes.filter(Boolean) : [];
+      // 200 但没给码 = 生成失败：不能当成功渲染（否则又是一次「空成功」）
+      if (!codes.length) throw new Error("no recovery codes returned");
     } catch (e) {
+      console.error("[recovery] regenerate failed:", e);
       toast(
-        t("security.recovery.toast_regen_failed", "重新生成失败：{msg}").replace("{msg}", e.message || e),
+        t("security.recovery.toast_regen_failed", "生成失败，请重试"),
         true,
       );
```

## 4. 测试（`scripts/e2e/suites/recovery_next.mjs`）

同一个套件（real authority：真 server + 真 Chrome + 真会话）新增两段，共 11 条新断言：

**失败路径**（`page.route` 把 `GET /v1/auth/recovery/status` 与 `POST /v1/auth/recovery/regenerate` 都打成 500）：

- 加载后：`#recovery-codes` 里没有 `code` 元素、网格文本为空、整页无「请立即保存 / 立即复制保存」、无 `DEMO`。
- 点「重新生成」+ 确认后：**必须真的发出 regenerate 请求**（`regenCalls > 0`，这条直接钉死「本地造码」回归）、toast 是「生成失败，请重试」、不出现「已生成新恢复码」、页面仍无任何码、仍无「请立即保存」。

**成功路径**（不 mock，走真后端）：点「重新生成」+ 确认 → `#recovery-codes code` 恰好 8 枚、互不相同、非 `DEMO` 形状；提示「请立即复制保存」、成功 toast 出现「请立即保存」。

绿输出（`./scripts/e2e/run_all.sh recovery_next`）：

```text
  --- #453 状态接口 500：页面不出现任何码 ---
    ✅ #453 状态失败：不渲染任何码
    ✅ #453 状态失败：无「请立即保存」提示
    ✅ #453 状态失败：无 DEMO 占位码
    ✅ #453 生成失败：toast 是「生成失败，请重试」
    ✅ #453 生成失败：没有「已生成/请立即保存」的假成功
    ✅ #453 生成失败：真的发出了 regenerate 请求（不是本地造码）
    ✅ #453 生成失败：页面仍无任何码
    ✅ #453 生成失败：无「请立即保存」提示
  --- 成功路径：8 枚恢复码 ---
    ✅ #453 成功路径：渲染 8 枚恢复码
    ✅ #453 成功路径：提示「请立即复制保存」
    ✅ #453 成功路径：成功 toast 出现「请立即保存」

  [recovery_next] 16 通过 / 0 失败
  ✅ 套件 recovery_next 通过
```

## 5. 变异验证（把假数据路径加回 → 断言必红）

变异做法：在 `regenRecoveryCodes()` 的 `try {` 之后原样插回被删的那段（与 §1 的原文一致）：

```diff
@@ -2159,6 +2159,25 @@
       btn.textContent = t("security.recovery.regenerating", "生成中…");
     }
     try {
+      if (!recoveryStatus) {
+        // MUTATION（#453 变异验证：把被删掉的假数据路径原样加回）
+        recoveryPlaintext = Array.from({ length: 8 }, (_, i) => {
+          const raw = ("DEMO" + (i + 1) + "CODEXXXXXXXX").slice(0, 16);
+          return (raw.slice(0, 4) + "-" + raw.slice(4, 8) + "-" +
+                  raw.slice(8, 12) + "-" + raw.slice(12, 16)).toUpperCase();
+        });
+        recoveryStatus = { unusedCount: 8, hasCodes: true };
+        renderRecovery();
+        toast(t("security.recovery.toast_regen", "已生成新恢复码，请立即保存"));
+        return;
+      }
       const res = await fetch(API + "/auth/recovery/regenerate", {
```

重建前端（`make fe`）后重跑同一套件：**5 条断言红**，套件 exit 1：

```text
    ❌ #453 生成失败：toast 是「生成失败，请重试」 — 已生成新恢复码，请立即保存
    ❌ #453 生成失败：没有「已生成/请立即保存」的假成功 — 已生成新恢复码，请立即保存
    ❌ #453 生成失败：真的发出了 regenerate 请求（不是本地造码） — regenCalls=0
    ❌ #453 生成失败：页面仍无任何码 — {"codes":8,"gridText":"DEMO-1COD-EXXX-XXXX复制DEMO-2COD-EXXX-XXXX复制…"}
    ❌ #453 生成失败：无「请立即保存」提示 — "…安全与登录…"

  [recovery_next] 11 通过 / 5 失败
  ❌ 套件 recovery_next 失败
```

`regenCalls=0` 与 `codes:8` 同时出现，就是事故现场：请求没发出、页面上却有 8 枚假码。

## 6. 门禁

| 门禁 | 结果 | 复算 |
| --- | --- | --- |
| `make ci` | ✅ exit 0 | `make ci` |
| 定向 e2e `recovery_next` | ✅ 16 通过 / 0 失败 | `./scripts/e2e/run_all.sh recovery_next` |
| 全量 e2e | ✅ 28/28 套件、2369 断言通过 / 0 失败（367.5s，`E2E_CONCURRENCY=3`，与另一个 worktree 的 e2e 并行跑） | `./scripts/e2e/run_all.sh` |
| 变异反证 | ✅ 5 条红 | 见 §5 |

说明：本机 `/tmp` 是 7.5G tmpfs，跑 `make ci` 时已用 80%（别的 worktree 留下的 `legion-node-copy-*`），第一次 `go test` 在链接阶段报 `disk quota exceeded` —— **环境问题，不是仓库问题**；把 `TMPDIR` 指到 `/home` 下同一命令 exit 0。

## 7. 相邻问题（本 issue 不改，留给对应 issue）

1. 状态接口失败时页面只有一行「无法加载恢复码状态」，没有可点的「重试」；下拉刷新能重试，但入口不明显。
2. 生成失败时若本页会话里已有上一轮**真实**明文，旧码仍留在页面上（真实码仍可用，清掉反而让用户丢码）——失败语义只体现在 toast 上。是否要在失败时也明确标注「上面是上一次的码」，属于 UX 决定。
3. 同页其它数据面（Passkey / 会话 / 密码状态）已在 #273 收敛到「失败就报错 + 重试」，恢复码这条是最后一个页面内假数据分支（已在 §2 记录）。

## 8. 复算命令

```sh
make fe                                             # sitegen + 指纹（e2e 前置）
./scripts/e2e/run_all.sh recovery_next              # 定向：16/0
make ci                                             # PR 门禁
```
