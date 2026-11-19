> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 本报告里的数字是本次修复落地时的定格（复算命令见 §6） -->

# 节点 YAML 编辑保存静默清空 `tags`（#454）

> 节点带 `tags` → 打开节点页 → 动一下别的表单字段 → 保存 YAML → **`tags` 变空**，页面不报错、不提示。

P1 数据丢失：用户没有做任何与标签有关的操作，标签被删掉了。修的是**前端**（`web-src/pages/node.html`）；
服务端的 merge 语义本来就是对的，不需要动。

## 1. 根因（核实结果，不是转述）

| # | 事实 | 证据 |
| --- | --- | --- |
| ① | `GET /v1/nodes/{name}/config` 是 W5 wrapper，**不投影 `tags`** | `internal/server/resource_node.go:856` `nodeConfigWrap`：只有 `metadata/namespaces/vars/limits` + `configRevision/appliedRevision/appliedHash` |
| ② | 页面的 YAML 草稿把 `config.tags` 当现值，缺失时兜底成**空数组** | 修前 `web-src/pages/node.html:313` `spec: { namespaces, vars, limits, tags: config.tags || [] }` |
| ③ | 草稿被序列化成 YAML 文本，`tags` 一律写出来（空数组写成 `tags: []`） | 修前 `web-src/pages/node.html:303` `lines.push(...yamlList('tags', Array.isArray(spec.tags) ? spec.tags : [], 2));` |
| ④ | 两条写入口都把**显式空数组**解释成「清空」 | `internal/server/resource_node_yaml.go:85` `nodeYAMLSpecPatch.Tags *[]string`（nil = 不动，`[]` = 清空）；`internal/server/resource_node.go:134` `nodeSpecPatch.Tags` 同一套 merge 语义 |
| ⑤ | 触发点是**任何**表单改动：`syncYAMLFromForm()` 会从草稿重建 YAML 文本 | `web-src/pages/node.html`：`configInputs` 的 `input` 监听、`#node-config-save`、`#node-yaml-reset` 都调它 |

于是「读面没告诉我」被写成了「用户清空了」：

```
config.tags         === undefined      // ①  never projected
config.tags || []   === []             // ②  "unknown" folded into "empty"
yamlList('tags',[]) === 'tags: []'     // ③  explicit empty in the document
PUT …/yaml {tags: []}  ⇒  n.Tags = []  // ④  server clears the column
```

**为什么 mock 预览看不出来**：假服务端的 `GET …/config` 里也没有 `tags`，但 mock 的
`PUT …/yaml` 是刻意不实现的（501 `yaml_write_unavailable`，见 `web/mock.js`），保存这一步在预览里
根本走不到服务端，缺陷（写空）就永远没发生。只有真服务 + 真点击才复现。

**为什么不是「用户没保存」**：`#node-yaml-input` 里那份文本就是写请求的正文；页面自己在表单改动时
把 `tags: []` 填了进去，用户看到的是 `tags: []`（如果不展开折叠块，他连看都看不到）。

## 2. 修法：回读现值 + 未知就整键省略（两条防线）

修后的三处（`web-src/pages/node.html`）：

```js
// ① 现值只从**权威读面**来：GET /v1/nodes/{name} → spec.tags。拿不到就是 null = 未知。
let knownTags = null;
// … loadFacts()：
knownTags = Array.isArray(resource.spec?.tags) ? resource.spec.tags.slice() : null;

// ② 草稿契约：config.tags（若将来 /config 真的投影了它）优先，否则用权威读面的现值。
spec: { namespaces, vars, limits, tags: Array.isArray(config.tags) ? config.tags : knownTags },

// ③ 序列化：**只有真的知道现值**才写这个键；未知就整键省略。
if (Array.isArray(spec.tags)) lines.push(...yamlList('tags', spec.tags, 2));
```

两条防线各自独立成立：

- **回读现值**（主路）：`GET /v1/nodes/{name}` 的 `spec.tags` 与 YAML 文档是同一份事实
  （`internal/server/resource_node.go:832` `nodeNonNilStrings(n.Tags)`），页面在 `loadFacts()` 里
  本来就拿到了这份响应 —— 不需要新请求、不需要在浏览器里解析 YAML（页面「只序列化、从不解析」
  的设计不变）。
- **整键省略**（兜底）：万一两个读面都没给（响应缺字段 / 加载失败 / 将来有人改投影），
  文本里就**不出现** `tags` 键。服务端对「省略」的解释是 merge-patch 的「不动」
  （`nodeYAMLSpecPatch.Tags *[]string`，nil），所以原值被保留 —— 而不是被清空。

明确的**非目标**：不做「把 `tags` 在 UI 里写死成 `[]`」，也不加一句提示了事（issue 的禁项）。

## 3. 服务端语义（为什么「省略」是安全的那一条）

`internal/server/resource_node_yaml.go:85`：

```go
type nodeYAMLSpecPatch struct {
	Namespaces *[]string         `json:"namespaces" yaml:"namespaces"`
	Vars       map[string]string `json:"vars" yaml:"vars"`
	Limits     map[string]any    `json:"limits" yaml:"limits"`
	Tags       *[]string         `json:"tags" yaml:"tags"`   // nil = 这一项不动；显式 [] = 清空
}
```

`TestNodeYAMLPutOmittedFieldsKeepValues`（`internal/server/resource_node_yaml_test.go`）原本只钉住
namespaces / limits，本次补上 `tags` 的两条**互相对照**的断言：

- 文档里省略 `tags` → 节点 `spec.tags` **不变**（页面兜底路径依赖的正是这条）；
- 文档里显式 `tags: []` → 节点 `spec.tags` 真的被清空（否则「省略」与「清空」不可区分，
  页面侧的修复就只是把缺陷挪了个地方）。

## 4. 测试（三层，全部可重跑）

### 4.1 单测：启动**真实页面控制器**（`node:test`，`scripts/v4-frontend.test.mjs`）

`bootNodeYAMLPage()` 把 `web-src/pages/node.html` 的 inline script 真的读进 `vm` 里跑，
`Anotify.model` 用**真的** `web/partials.js`（`spec.tags` 的投影就发生在那里），只把传输
（`api/apiList`）与布局（`mountLayout`）换成桩。喂的是两个读面的**真实形状**：

- `GET /v1/nodes/anode-1` → `spec.tags: ['gpu','prod']`
- `GET /v1/nodes/anode-1/config` → **没有** `tags` 键（W5 wrapper 的原样）

三条断言，分别对应三条不能破的语义：

| 用例 | 断言 |
| --- | --- |
| 权威读面有 tags | 表单 `input` 后草稿里是 `  tags:\n    - gpu\n    - prod`；点保存发出的 PUT 正文里也有 |
| 两个读面都没有 tags | 草稿里**不存在** `tags` 键（整键省略），PUT 正文里也没有 |
| 权威读面明确是 `[]` | 草稿里照旧写 `  tags: []`（「已知的空」不许被吞掉） |

### 4.2 e2e：真实服务 + 真 Chrome（`frontend` 套件的 `node-config` 案例）

`scripts/e2e/fixtures/f3/f3-node-retained.mjs` 的 `config` 模式追加一段（真实权威分支）：

1. 用真实写入口 `PATCH /v1/nodes/anode-1`（merge-patch 信封）把 `spec.tags` 设为 `['gpu','prod']`；
2. 断言 `GET …/config` 的响应里**没有** `tags` 键（把缺陷根因钉成事实，不是猜测）；
3. 重新打开页面（走一次真实 boot）→ 改表单（`input` 事件）→ 读 `#node-yaml-input`：
   必须是 `  tags:\n    - gpu\n    - prod`；
4. 点真实按钮 `#node-yaml-save` → `#node-yaml-state` 变 `Saved…` → 回读
   `GET /v1/nodes/anode-1` 的 `spec.tags` **仍是** `['gpu','prod']`，且 YAML 读面里还有 tags。

### 4.3 Go：merge 语义（`make test` 覆盖）

见 §3。

## 5. 变异证据（改回缺陷写法 → 断言红）

### 5.1 变异 A：`draftDoc()` 改回 `config.tags || []`（issue 里点名的那一处）

```
$ python3 - <<'PY'   # 把 web-src/pages/node.html 的
  …                  # tags: Array.isArray(config.tags) ? config.tags : knownTags
  PY                 # 改回       tags: config.tags || []
$ node --test --test-name-pattern '454' scripts/v4-frontend.test.mjs
✖ #454 node YAML draft keeps authoritative spec.tags when /config omits the field
✖ #454 node YAML draft omits tags entirely when no read面 carries the current value
✔ #454 node YAML draft still writes an explicit empty tags list the server actually reports
ℹ tests 3
ℹ pass 1
ℹ fail 2
    tags: []
    tags: []
  '  tags: []' !== ''
    actual: '  tags: []',
```

### 5.2 变异 B：`docToYAML()` 改回「未知也写 `tags: []`」

```
$ # if (Array.isArray(spec.tags)) …  →  lines.push(...yamlList('tags', Array.isArray(spec.tags) ? spec.tags : [], 2));
$ node --test --test-name-pattern '454' scripts/v4-frontend.test.mjs
✔ #454 node YAML draft keeps authoritative spec.tags when /config omits the field
✖ #454 node YAML draft omits tags entirely when no read面 carries the current value
✔ #454 node YAML draft still writes an explicit empty tags list the server actually reports
ℹ tests 3
ℹ pass 2
ℹ fail 1
```

### 5.3 变异 C：e2e（真实服务 + 真 Chrome，同一处 `|| []`）

把 `draftDoc()` 改回 `config.tags || []`，`make fe` + 重建二进制后单跑该案例：

```
$ node scripts/e2e/suites/frontend.mjs --case node-config
  ❌ node-config — AssertionError [ERR_ASSERTION]: YAML 草稿必须带 tags（把 `|| []` 写回去这条必红）：
  tags: []
  - node-config — AssertionError [ERR_ASSERTION]: YAML 草稿必须带 tags（把 `|| []` 写回去这条必红）：
  tags: []
```

恢复修复（`make fe` + 重建）后同一条命令：

```
$ node scripts/e2e/suites/frontend.mjs --case node-config
  ✅ node-config (2 behavior assertions)
```

## 6. 门禁（命令 + 数字）

### 6.1 受影响 e2e（真实服务 + 真 Chrome）

`web-src/pages/node.html` 在 `scripts/e2e/affected-map.mjs` 里命中 `frontend`（`pages` 含 `node`），
`node-config` 是唯一走节点页 YAML 编辑面的案例。

```
$ ./scripts/e2e/run_all.sh frontend
✅ dist 与源一致：stamp 522cd52fcce0…
  ✅ 套件 frontend 通过
  总耗时: 149.6s
  断言通过: 42
  断言失败: 0
  ✅ frontend           42通过/0失败  149.2s  mode=real
✅ E2E 全绿：1/1 个请求套件均达标（断言 **42 条**通过 / 0 条失败）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 149.7s）
```

（`node-config` 单跑重跑命令：先 `./scripts/e2e/run_all.sh frontend` 生成二进制，然后
`ANOTIFY_BIN=$PWD/anotify DEVSEED_BIN=$PWD/.e2e-bin/devseed ANODE_BIN=$PWD/.e2e-bin/anode \
node scripts/e2e/suites/frontend.mjs --case node-config`。）

同一命令在**已提交的树**上复跑：`✅ frontend 42通过/0失败 171.4s mode=real`
（`断言 42 条通过 / 0 条失败`，断言数与 authority 一致）。

### 6.2 `make ci`

```
$ make ci
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
✅ 模块约定与代码一致（check-agents：6 个 area）
✅ class 校验通过：877 个类（HTML class 属性 628 + JS class 串新增 249）
✅ 契约字段守卫通过（234 个 schema、724 个已知字段）
✅ CSS 结构守卫通过：3 文件 / 863 规则 / 49 at-rule 块 / 3094 声明
共 48 项判定：PASS 48 / FAIL 0            # 对比度（tokens 双主题）
✅ 测试纪律棘轮、✅ 注解即外键守卫通过、✅ REF_DEAD 0、✅ 改动→套件映射守卫通过
go vet ./... / go build ./... / go build -tags embeddist（anotify + anode）
go test ./... ：41 个包 ok（0 FAIL，6 个 no test files）
✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 101.8s）
BenchmarkDispatchChain-12  1  2299128 ns/op  124048 B/op  2284 allocs/op
✅ harness 自测通过
（make ci 退出码 0；本次运行里没有一条 `make: *** Error`）
```

### 6.3 单元测试文件（不在 `make ci` 里，手动跑）

```
$ node --test scripts/v4-frontend.test.mjs      # 本 worktree，基线 = origin/main
基线（HEAD 未含本次改动）：ℹ tests 218 · pass 195 · fail 23
本次（修复 + 3 条新用例）：ℹ tests 221 · pass 198 · fail 23
相对基线：新增红 **0 条**、修掉 **0 条**（红集合逐条相同，都是要活 server / 真 Chrome 的用例）
```

## 7. 相邻问题（已登记，本 PR 不改）

1. **`limits.total: null` 的「取 → 贴回」不幂等**（P2，真实复现于本次 e2e）：节点没有
   `limits.total`（合法状态，`{}`）时，服务端 YAML 读面按设计写 `total: null`
   （`internal/server/resource_node_yaml.go:80` 的注释：「未写 → 显式 `total: null`」），
   但把它原样写回会被 `store.ValidateLimits` 拒成 `400 value_invalid`
   （`internal/store/config.go:203`「limits.total 必须是 ≥1 的整数，实际 <nil>」）。
   也就是**服务端自己的规范文档读回来存不回去**。本 PR 不修（超出 #454 的边界），
   只在 e2e 里给节点补一份合法 `limits`，把断言聚焦在 tags 上。建议单独开 issue：
   要么序列化时省略 `total`，要么 `ValidateLimits` 接受显式 `null` 为「不设上限」。
2. **`/config` 不投影 `tags`（W5 wrapper）本身没有错**：它明确是「三个可写键 + revision
   三兄弟」的配置视图，`tags` 属于 Node 的 `spec`。但页面把它当成了「节点配置的唯一来源」，
   这正是 #454 的结构性根因。若将来要给节点页加标签编辑器，建议走
   `PATCH /v1/nodes/{name}`（`spec.tags`）而不是把它塞进 `/config`。
3. **`PUT …/config` 是「全量替换」**：只给 `vars` 的 PUT 会把 `namespaces` / `limits` 一起清掉
   （本次 e2e 里那个 `PUT {metadata, vars}` 就是这么把 limits 变成 `{}` 的）。契约如此
   （`api/resource.yaml · putNodeConfig`「整体替换」），但页面用的是「表单里三块都带上」的写法，
   所以人不会踩到；只有在脚本/测试里写局部 body 才会。不改，只记录。
4. **两条与 #454 无关的 e2e flake（本次实测撞到，均有复跑证据）**：
   - `frontend · node-aba`（`namespace.html` 的 ABA 案例）：偶发 `__f3Calls.length` 19 ≠ 18
     （多一次请求落进了 50ms 的事件循环屏障）。单跑 `--case node-aba` **3/3 绿**；
     全量并行 4 次里 2 次红、2 次绿（本报告 §6.1 记的是绿的那两次）。
   - `cmd/anode/internal/runner · TestParentPositiveLeaseTimerRegistrationCrossingAdvance/register_at_61s`：
     全量 `make test`（包级并行 + shuffle）下偶发「等待 real executions awaiting result ACK 超时」
     （2.00s 窗口打满）。单跑 `-run` **3/3 绿**（0.02s）。两条都不在 #454 的改动面上，
     按 issue 边界只记录、不修。

## 8. 风险与未做的事

- 页面**没有**标签编辑器（只有 YAML 文本）。本次不新增编辑器：那会把「表单 → YAML」的一致性
  问题扩大成两份可写事实。改标签仍然只有 YAML 一条路（现在它不再丢数据了）。
- `knownTags` 在 `loadFacts()` 里刷新：YAML 保存成功、config 保存成功、刷新按钮、重进页面都会走它；
  只有「另开一个标签页改了 tags，本页不动」这一种情况会拿到旧值（与 `#node-machine-limits`
  等所有其它字段同档，不是本次引入的）。
- 未改：`web/mock.js`（预览里 `PUT …/yaml` 仍是 501），因此 e2e 的**保存**断言只在真实权威分支跑；
  mock 分支断言草稿文本（`tags` 在草稿里），两条路径互不替代。
