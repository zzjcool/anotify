# 生成产物只写不删 → 删掉的页面仍被 embed（#649）

snapshot-doc: 2026-11-25 #649 修前/修后取证；本文数字是当次实测定格，复算命令逐节给出

基线：`4e096e32`（修复前的 main）。交付分支：`pi-subagent/host-workspace/anotify-649-dist-2a8c8f18`。
事故形态：dev 上 `/receivers.html` 与 `/sw.js` 返回 200，而这两个源已被退役（推送域 V4-D7）。

## 1. 根因（实测校正过三处，和 issue 里的描述不完全一样）

issue 写的链条是「sitegen 只写不删 → hash.mjs 只写不删 → embed 收全量」。逐条核对后：

| 环节 | issue 的描述 | 实测 | 结论 |
| --- | --- | --- | --- |
| `sitegen`（web-src/pages → web/*.html） | 只写不删 | **已会 prune**（`internal/sitegen/sitegen.go` 的 `pruneStalePages`，`f611a249` 起） | 本地删页面时 `web/<page>.html` 会被清掉 |
| `scripts/hash.mjs`（web/ → dist/） | 只写不删 | 整目录删（`fs.rm(out, {recursive:true, force:true})`） | 本地 `make fe` 因此"碰巧"干净 —— 代价是**任何** dist 里的东西都被静默销毁（含别的阶段的产物） |
| 守卫（`check-dist-fresh`） | 只比源指纹，多出来的文件不在判定范围 | **确认** | 见 §2 修前取证 |
| 镜像构建 | 未提 | **这是部署上真的漏的那一处** | 见 §2.3 |

所以线上 200 的真正通路是**部署路径**，不是本机 `make fe`：

```
Dockerfile build 阶段：COPY . .                              → 把宿主 gitignore 的 internal/server/dist 带进镜像
                       COPY --from=fe …/dist ./internal/server/dist → 只覆盖同名文件，**不删**多出来的旧产物
go build -tags embeddist：//go:embed all:dist                 → 旧页面/sw.js 一起进二进制
```

`COPY --from` 的合并语义（而不是替换目录）是这条通路的机关。合成探针（两阶段 + 同形 COPY）：

```text
COPY . /app                      → /app/internal/server/dist/sw.js（宿主陈旧产物）
COPY --from=fe /out /app/…/dist  → ls 结果：index.html 与 sw.js 并存
```

## 2. 修前取证（4e096e32）

### 2.1 守卫对"无源残留"静默放行

```text
$ printf '<h1>stale</h1>\n' > internal/server/dist/ghost-page.html
$ node scripts/dist-stamp.mjs --check
输入面 70 个文件：文本 55 / 二进制 15（已计入 sha256）/ 超大跳过 0
✅ dist 与源一致：stamp 6bff99749259…
$ echo $?
0
```

塞进 dist 的 `ghost-page.html` 与 `sw.js` 既不在源里、也不在 stamp 的判定面里 —— 判定面只有**输入**指纹。

### 2.2 `make fe` 的"干净"来自整目录删

```text
$ grep -n 'fs.rm(out' scripts/hash.mjs        # 修前
	await fs.rm(out, { recursive: true, force: true });
```

它不是 prune：`dist/.source-stamp`（下一个阶段才写）在流水线中途被删掉再重建；任何将来落在 dist 里的
其它阶段产物同样会被无声销毁。**删页面 → 再 make fe ⇒ dist 干净**这条验收因此在修前就是绿的，
但绿的理由是"整个目录重来"，不是"只删该删的"。

### 2.3 部署路径：真镜像里旧页面确实 200

用 `git archive 4e096e32` 建镜像，并在构建上下文里预置"宿主机陈旧 dist"
（`receivers.html` + `sw.js`，正是事故现场的形态），起容器后取状态码与响应体：

```text
===== port 18081  PRE  (4e096e32, no fix) =====
/receivers.html  -> 200
/sw.js           -> 200
/index.html      -> 301
-- /receivers.html body --   <h1>stale</h1>
-- /sw.js body --            self.addEventListener("fetch",()=>{});
```

## 3. 修了什么

### 3.1 prune（替代整目录删）

`scripts/hash.mjs` 写入前不再 `fs.rm(out)`：它记下本轮实际产出的路径集合，写完 manifest 后自底向上
删掉"本轮没产出且不属于其它阶段"的文件，并**逐个打印**：

```text
$ git mv web-src/pages/release.html web-src/pages/release-notes.html && make fe
   · 清掉无源残留：en/release.html
   · 清掉无源残留：es/release.html
   · 清掉无源残留：ja/release.html
   · 清掉无源残留：release.html
✅ 指纹完成：33 个资源，88 个 HTML → …/internal/server/dist
```

`dist/.source-stamp` 由 `OWNED_BY_OTHER_STAGES` 显式保留 —— 这正是"不要整目录删"的原因。
同样的两个文件就是事故现场的残留名；把 `receivers.html` + `sw.js` 放回 dist 再跑一遍：

```text
$ node scripts/hash.mjs web internal/server/dist
   · 清掉无源残留：receivers.html
   · 清掉无源残留：sw.js
```

真实磁盘上的这套行为有单测钉着（`scripts/hash.test.mjs`，接在 `make test-frontend`）：
残留消失 / `.source-stamp` 保留 / 删源页面后再跑就不再有它 / **写盘集与 `expectedDistPaths` 的期望集逐路径相等**
（两侧各演一套是同一类缝隙的下一段）。

### 3.2 守卫：dist 里"源已不存在"的文件判红并点名

`scripts/dist-stamp.mjs` 增两条规则码（都由 `scripts/hash.mjs` 的 `expectedDistPaths` 推导期望集，
判据只有一份，不在两个文件里各写一遍）：

```text
$ printf '<h1>stale</h1>\n' > internal/server/dist/ghost-page.html
$ printf 'self.addEventListener("fetch",()=>{});\n' > internal/server/dist/sw.js
$ make check-dist-fresh          # 内含 --self-test
输入面 70 个文件：文本 55 / 二进制 15（已计入 sha256）/ 超大跳过 0
❌ internal/server/dist 里有「源已不存在、本轮也不会产出」的残留（它们会被 go:embed 编进二进制）：
     - ghost-page.html
     - sw.js
   为什么不该在：dist 是 web/ 的指纹产物，上面这些路径在 web/ 与 manifest.json 里都找不到来源
   修法：make fe      # 重新生成；hash.mjs 只删本轮没产出的文件，并逐个打印
make: *** [Makefile:112: check-dist-fresh] Error 1
```

另一侧同样判：缺本轮该产出的文件、`manifest.json` 读不出、源该指纹却无 manifest 条目 ⇒ `DIST_INCOMPLETE`。
`--write` 也会先判一次 —— 脏 dist 不许被盖上 stamp（"门禁把脏产物认证成新鲜"是同一类缝隙）。

`make ci` 现在在 `fe` 之后跑 `make check-dist-fresh`（此前它只在 `scripts/e2e/run_all.sh` 前置里跑）。

### 3.3 本地清理入口

`make fe-clean` 清掉 `web/*.html` / `web/{en,es,ja}` / `web/i18n.*.js` / `internal/server/dist`，
README 的「删掉/改名页面之后」一节写清了日常该怎么走（prune 自愈 → 守卫点名 → 整份重置）。

### 3.4 镜像构建上下文

`.dockerignore` 排除 `internal/server/dist`：不给 `COPY . .` 把宿主产物带进镜像的机会，
部署路径上就不再有"旧页面随 embed 进二进制"的通道。同时 fe 阶段补了一行
`COPY scripts/lib/direct-run.mjs`：`scripts/hash.mjs` 改成库之后要 import 它，漏拷会让镜像构建在
fe 阶段直接 `ERR_MODULE_NOT_FOUND`（`docker build --target fe` 已跑过，见 §7）。

## 4. 验收逐条

| # | 判据 | 结果 |
| --- | --- | --- |
| 1 | 删/改名源页面 → `make fe` → `dist/<page>.html` 不存在 | 改名场景整链绿：`release.html` 在 root 与三个语言目录全部消失（§3.1 原文）；删除场景见 §5 变异 |
| 2 | 塞无源文件 → 守卫判红并点名 | 红，exit 1，点名 `ghost-page.html` / `sw.js`（§3.2 原文） |
| 3 | 不误伤：指纹资源 / `.source-stamp` / `manifest.webmanifest` 保留 | 修前修后 dist 清单**逐文件 diff 为空**（128 → 128），`make fe` 连跑两次 stamp 都是 `6bff99749259…` |
| 4 | 删掉的页面不再 200 | 真镜像：`/receivers.html` 200 → **404**，`/sw.js` 200 → **404**（§6） |
| 5 | 两条变异必红 | 见 §5 |

### 4.1 dist 文件数对比（修前 → 修后，同一棵树、同一份源）

```text
修前（4e096e32，make fe）：128 个文件
修后（本分支，make fe）：  128 个文件
diff（逐路径排序后比对）：  空
```

数字相同是**预期**：这份源里没有陈旧产物，prune 无事可做。差异只在"有残留时"的行为 ——
修前整目录重来（静默），修后只删多出来的并打印，且守卫会点名。

## 5. 变异自证（判据有牙）

**变异 1：去掉 prune**（`hash.mjs` 的 `pruneStaleOutputs` 调用改成空数组）

```text
$ rm web-src/pages/release.html && make sitegen && node scripts/hash.mjs web internal/server/dist
$ ls internal/server/dist/release.html internal/server/dist/en/release.html
-rw-r--r-- … internal/server/dist/en/release.html
-rw-r--r-- … internal/server/dist/release.html
```

验收 1 红：删掉的页面仍在 dist 里，下一次 `go build -tags embeddist` 就会把它编进二进制。

**变异 2：去掉守卫**（`judge` 里的 dist 形状判定短路）

```text
$ printf '<h1>stale</h1>\n' > internal/server/dist/ghost-page.html
$ node scripts/dist-stamp.mjs --check ; echo $?
✅ dist 与源一致：stamp 6bff99749259…
0
```

验收 2 红：无源残留静默通过。恢复守卫后同一命令 exit 1 并点名 `ghost-page.html`。

## 6. 部署路径修复的实证（真镜像）

同一份陈旧 dist 预置进构建上下文，只差一个提交（`c0944f33` 的 `.dockerignore`）：

```text
===== port 18081  PRE  (4e096e32) =====          ===== port 18082  POST (c0944f33) =====
/receivers.html  -> 200                          /receivers.html  -> 404
/sw.js           -> 200                          /sw.js           -> 404
/index.html      -> 301                          /index.html      -> 301
-- /receivers.html body -- <h1>stale</h1>        -- /receivers.html body -- 404 page not found
-- /sw.js body --          self.addEventListener  -- /sw.js body --          404 page not found
```

`/index.html` 的 301（→ `./`）两侧一致，说明差异只来自 dist 内容，不是服务端行为变了。

交付分支最终状态再跑一次（含 `.dockerignore` 与 Dockerfile 的 fe 阶段 COPY，宿主机 dist **故意塞回**
`receivers.html` + `sw.js`）：

```text
docker build -t w649-final .   → BUILD_EXIT=0
health=200
/receivers.html -> 404
/sw.js           -> 404
/nodes.html      -> 200        # 正常页面照常服务，不是把静态面整个关掉
```

## 7. 门禁数字

```text
make ci            → exit 0
  · go test ./...（-count=1 -shuffle=on）：42 个包 ok / 0 FAIL
  · 前端单测（node --test ×7 组）：285 tests / 285 pass / 0 fail（含 scripts/hash.test.mjs 的 4 条）
  · 对比度复算：48 项判定 PASS 48 / FAIL 0
  · make fe 写 stamp 6bff99749259…；紧随其后的 make check-dist-fresh → ✅ dist 与源一致

make e2e-affected  → exit 0
  · 全量矩阵 31/31 套件均达标 · 断言 3544 通过 / 0 失败 · 总耗时 427.9s
  · 套件级重试 0 · case 级 timeout/incomplete 重试 0 · 临时目录零残留
  · 前置段：✅ dist 与源一致（stamp 6bff99749259…）→ 构建二进制 → 开跑

docker build --target fe .  → exit 0（fe 阶段的 dist：22 个注页面 + 资源，无 .source-stamp/无 sw.js）
```

`make e2e-affected` 把本次改动判成**全量矩阵**（`scripts/dist-stamp.mjs` 落在共享面：它是 e2e 前置的产物新鲜度门禁），
所以这条命令真的跑了全量套件而不是抽检 —— 这正是本次想要的那种“不被选择逻辑遮住”的结果。

## 8. 没做 / 残留风险

- **`web/` 本身没有守卫**：dist 的期望集从 `web/` 推，而 sitegen 只 prune `.html`（`internal/sitegen/sitegen.go`
  的 `pruneStalePages` 只认 `.html`），所以 `web/` 里"非 html 的陈旧生成物"、或 `-langs` 缩短后留下的语言目录，
  会被写入端与守卫**同时**当成合法源 —— 对这类残留恒绿。要让这条缝也机检，得先把 `web/` 的生成物表
  与 `web-src/pages` 对账（本轮未做，避免把 `.gitignore` 的清单复制成第二份真相）。
- 镜像里的 `web/` 仍来自宿主 `COPY web/`；陈旧的生成物靠 sitegen 的 prune 兜，陈旧的**非**生成物
  （比如曾入库后被删的 `web/sw.js`）靠 git 检出来保证 —— 这条链没有机检，属残留风险。
- `.dockerignore` 与 `.gitignore` 的生成物清单是两份知识；目前只有 dist 这一条会真的进二进制，
  所以只排了它，没有把整张表复制过去。
- 判据顺序：`DIST_ORPHAN` 排在 `DIST_STALE` 之前 —— 两者同时成立时只看到残留清单，看不到逐文件 stale diff
  （再跑一次 `make fe` 两者都消失，所以只是噪音，不是错判）。
