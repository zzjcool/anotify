> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 本报告里的数字是本次交付落地时的定格（复算命令见 §8） -->

# 所有资源共用一份 YAML 文档层（#473）

> 症状：同一份资源文档由四个地方各拼一份（节点页、命名空间页、vars 表格、预览 mock），
> 每份的标量转义、空集合形态、字段顺序各自演化。于是同一类缺陷在每个资源上反复长出来 ——
> `#454` 的 `tags` 被静默清空就是「页面自己拼一份、拼的时候漏了一个字段」，而运行期没有
> 任何报错：页面 200、保存 200，只有用户看到的值对不上。

`#473` 拆成三步。第 2 步（唯一写入口 `PUT …/yaml`）已由 `#480`/`#476` 落地；本报告交付
**第 1 步（一份文档层）** 与 **第 3 步（页面不再各写一份序列化 / 前导拼装）**。

## 1. 三步各自的状态与落点

| 步骤 | 状态 | 落点 | 证据 |
| --- | --- | --- | --- |
| ① 一份文档层（序列化 / 反序列化 / 字段顺序 / 标量转义） | 本次交付 | `web/yamldoc.js`（唯一实现，见 §2） | `git grep -c "yamlScalar" -- web web-src` 0 命中；`scripts/check-yaml-doc.mjs` 的 `dup-serializer` 判据 |
| ② 唯一写入口 `PUT …/yaml` | 已在 `#480`/`#476` 落地，本次未改语义 | `web-src/pages/node.html:473`、`web-src/pages/namespace.html:154`、`web-src/pages/admin.html` 的系统保留期例外 | `make check-config-writes`（≈0 处资源级 `/config` 写）；e2e `vars_ui` 的「保存请求只打到 …/yaml」 |
| ③ 统一组件/模板（可视化表单与原始编辑共用同一份文档） | 本次交付 | 两个页面都只调 `YD.edit` / `YD.render` / `YD.path`（见 §3 调用点表） | `scripts/yamldoc.test.mjs` 的「页面接线」用例 + `check-yaml-doc` 的 `doc-literal` 判据 |

## 2. 第 1 步：`web/yamldoc.js` 的 API 与规则

| 出口 | 行 | 作用 |
| --- | --- | --- |
| `scalar(v)` | `web/yamldoc.js:22` | **唯一一份**标量转义：安全形状裸写，其余写 JSON 字符串（JSON 的双引号标量就是合法 YAML） |
| `render(kind, model)` | `web/yamldoc.js:291` | **唯一一份**「模型 → 文档文本」：块映射/块序列/空集合 `{}`·`[]`/块标量 `\|`/多行正文 |
| `parse(kind, text)` | `web/yamldoc.js:614` | **唯一一份**「文档文本 → 模型」：块映射、块序列、流式标量集合、单双引号、块标量、注释；拒绝集 = 折行块 `>`、锚点/别名、自定义 tag、多文档、复杂键、重复键、制表符缩进 |
| `edit(kind, text, patch)` | `web/yamldoc.js:660` | 表单改一处的**完整动作**：读服务端原文 → 只改 patch 的键 → 重新序列化。`problems` 非空时返回空文本（调用方不得写） |
| `merge(base, patch)` | `web/yamldoc.js:628` | 通用深合并（只用于「从零攒一份」的兜底路径） |
| `patchObject` | `web/yamldoc.js:644` | 按 kind 表分层打补丁：**结构**节点（`spec`/`metadata`/`limits`）递归，**值**节点（`vars`/`perCapability`/`namespaces`/`tags`）整份替换 |
| `KINDS` | `web/yamldoc.js:162` | 每种文档的字段顺序与省略规则（`Node` / `Namespace` / `Capability` / `Source` / `AnodeConfig`） |
| `path(kind, name)` | `web/yamldoc.js:678` | 该资源的文档端点（写入口永远是这个 `…/yaml`） |

四条规则值得单独记下来（改之前先读）：

- **读面与写面的唯一差异是「未知就省略」。** 服务端读面为了表达「不压住下层」会写
  `total: null`（`internal/server/resource_node_yaml.go` 的 `nodeYAMLLimits.Total` 是 `*int`），
  而写面要求 total 是 ≥1 的整数（`internal/store/config.go` 的 `ValidateLimits`）。所以模型里
  total 为 null 时**整键省略**（省略 = 不动），绝不写 `total: null` / `total: 0`。
- **「未知」与「已知为空」必须可区分。** `spec.tags` 为 null → 整键省略；`tags: []` → 照写
  `tags: []`。这是 `#454` 的判据，也是本轮所有保真断言的基线。
- **值节点整份替换，不合并。** 表格里删掉一行时，patch 里就没有那个键；如果按对象合并，
  删除会被读成「没动」—— 那是 `#454` 的另一面（静默保留 vs 静默清空）。
- **扩展字段原样带回。** kind 表之外的键按模型顺序追加在声明字段之后；服务端读面多一个键，
  页面保存时不会把它丢掉（真写不进去时由服务端的严格解析具名拒绝，而不是悄悄消失）。
- **块标量的 chomping 必须与 yaml.v3 一致。** 多行值按「末尾换行数」选 `|-`（没有行尾换行）/
  `|`（恰好一个）/ `|+`（两个以上，多出来的空行照写），读的一侧三种都要认（含缩进指示器
  `|4-`）。写死 `|` 会给每个多行值补一个行尾换行 —— 证书 / JSON / 脚本片段就是这样被静默改写的。
  首行以空白开头、或正文里有 `\r` 的值不走块标量，改用双引号标量（`"a\nb"`）：那是精确形式，
  不依赖缩进推断。

`envPassthrough` 在 kind 表里是 **`strings`（名字白名单）**，`vars` 是 **`string-map`（名字→值）**：
类型不同，谁也不会被渲染成另一个的形状，`patchObject` 也不会把一个列表搬进映射。值本身不脱敏
（脱敏是服务端读投影的职责：`POST …/tokens` 一次性明文、`GET …/sources/{name}/yaml` 只给
`hookUrl`/`secretSet`）。

## 3. 第 3 步：调用点清单（证明不再有多份实现）

| 文件:行 | 调的是什么 | 改之前是什么 |
| --- | --- | --- |
| `web-src/layouts/base.html:113` | 每页同步加载 `/yamldoc.js` | —（没有这一层） |
| `web-src/pages/node.html:428` | `const YD = window.AnotifyYamlDoc` | — |
| `web-src/pages/node.html:430` | `YD.parse('Node', 服务端原文)` → 表单模型 | `draftDoc()`：从 `/config` 投影攒草稿，缺 `tags` |
| `web-src/pages/node.html:435` | `YD.edit('Node', 原文, {spec:{vars}})` → 表格保存的文本 | `docToYAML(draftDoc())`（页面自带的 4 个函数） |
| `web-src/pages/node.html:462` | `YD.render('Node', model)` → 「按当前配置重写」 | 同上 |
| `web-src/pages/node.html:453` / `:473` | `YD.path('Node', name)`（GET / PUT 同一个端点） | 手拼 `path + '/yaml'` |
| `web-src/pages/namespace.html:141` | `YD.edit('Namespace', 原文, {metadata:{…}, spec:{vars}})` | `namespaceYAMLText()`：手拼 apiVersion/kind/metadata/spec 行 |
| `web-src/pages/namespace.html:154` / `:494` | `YD.path('Namespace', scope)` | `nsYamlPath()` / 手拼字符串 |
| `web/mock.js:2680` | `renderDoc(kind, doc)`（薄适配，排版全交给文档层） | `toYaml` + `yamlScalar` + `yamlLines`（第三份发射器） |
| `web/mock.js:2686` / `:2694` / `:3171` / `:3226` | Capability / Source / Node / Namespace 四类文档 | 手拼行 + 自带 `nsYAMLScalar` |
| `web/vars-table.js:160` | 只有 `rowsToVars`（行 → 映射），**不再序列化** | `yamlScalar` + `varsToYAMLLines` |

复算（旧实现的三个名字在 `web/**` 里一处都不剩；注释按密度棘轮清掉之后连「历史上曾经
有过」的说明也不再保留 —— 行为以代码与 `scripts/yamldoc.test.mjs` 为准）：

```console
$ git grep -c "yamlScalar" -- web web-src        # 0 命中
$ git grep -c "docToYAML\|namespaceYAMLText\|varsToYAMLLines" -- web web-src   # 0 命中
$ git grep -c "AnotifyYamlDoc" -- web web-src
web-src/pages/namespace.html:1
web-src/pages/node.html:1
web/mock.js:1
web/yamldoc.js:1
```

## 4. 字段保真（读 → 表单编辑 → 写 → 再读）

同一条判据对每一种字段：**服务端原文 → `edit` 改一个键 → 再 `parse`**，除了被改的那个键，
其余路径逐字不变。测试在 `scripts/yamldoc.test.mjs`（`make test-frontend` 接线）。

| 字段 | 所在文档 | 断言 | 测试位置 |
| --- | --- | --- | --- |
| `tags` | Node | 有序原样；未知不补键；`[]` 不被吞 | `scripts/yamldoc.test.mjs:69`、`:120` |
| `limits.total` / `limits.perCapability` | Node | `total` 数字不变形；`perCapability` 的 `'*'` 键与值不变；`total: null` 写回时整键省略 | `scripts/yamldoc.test.mjs:69`、`:138` |
| `spec.vars` | Node / Namespace / AnodeConfig | 整份替换（删键真的删掉）+ 其余字段不动 | `scripts/yamldoc.test.mjs:69`、`:138`、`:211` |
| `spec.namespaces`、`metadata.resourceVersion` | Node | 逐字不变（顺序也算事实） | `scripts/yamldoc.test.mjs:69` |
| `requires.values`、`run.workDir`（含 `run.files` 块标量正文） | Capability | 改 `workDir` 不碰 `requires.values`，脚本正文逐字往返 | `scripts/yamldoc.test.mjs:181` |
| `envPassthrough` | AnodeConfig | 是名字数组（不是映射）；改 `vars` 不动它，也不会被搬进 `vars` | `scripts/yamldoc.test.mjs:262` |
| 未知扩展字段（`spec.futureKnob` / 顶层 `x-ext`） | Node | 原样带回（值逐字） | `scripts/yamldoc.test.mjs:163` |
| 多行值（块标量正文） | Capability / Node / Namespace | 末尾换行数逐一往返（0 / 1 / 2+）、行首空白、行内制表符、`\r\n`、只有换行的值；yaml.v3 的 `|-` / `|` / `|+` / `|4-` 都读得回来 | `scripts/yamldoc.test.mjs:211`（块标量用例） |
| 被上层压住的键 | Node / Namespace | 表格保存后，文档里已有、但 `local`/`node` 压住的键**原样保留**（不抄生效值、也不删）；用户显式删掉才消失 | `scripts/vars-table.test.mjs`（`rowsToVars` 的压住用例） |
| 真服务 + 真 Chrome 的同一条判据 | Node / Namespace | 表格保存后 `tags` / `limits` / 授权 `namespaces` 段逐字不变 | `scripts/e2e/suites/vars_ui.mjs:343`、`:346`、`:349`、`:420` |

e2e 的判据取**服务端两次 `GET …/yaml` 的原文**（不是页面发出的文本）：前端重排缩进或换引号会被
服务端归一化掉，这里钉的是「事实没被表单路径改掉」，而不是排版细节。

## 5. 边界：显式 `total: null` 原样贴回是 200（#570）

读面用显式 `total: null` 表达「本层不压住下层」。写面现在把已知维度的显式 null（`total`、
`perCapability`、`perCapability.X`）按「没设这一项」接住：值不进库，`GET …/yaml` 可以原样贴回；
`perCapability: {}` 也与「没设」同义（丢掉该键，写回后的行与写前逐字相同）。

```
GET  /v1/nodes/{name}/yaml   →   limits:\n    total: null      （读面语义：不压住下层）
PUT  /v1/nodes/{name}/yaml   ←   原样贴回                     →  200（除 resourceVersion 外逐字不变）
```

e2e `scripts/e2e/suites/vars_ui.mjs` 的同一个边界用例钉住 200、文档除 `resourceVersion` 外逐字
不变、读面仍写 `total: null`、以及 config 的 limits 不含 `total` 键。`0` / 负数 / 字符串仍是
400 `value_invalid`；非法 `perCapability` 键写成 null 也一样被拒（先校验再归一，不能借 null
绕过键名校验）。前端 `web/yamldoc.js` 的表格路径仍把 `total: null` 翻译成省略该键 —— 两种写法
现在都表示「不设置」，不再是一个 200 一个 400。

文档里完全没有 `limits` 键时，本层仍不凭空补空 `limits`（`scripts/yamldoc.test.mjs:138`）。

## 6. 前后端同一套规则

前端 kind 表不是凭印象写的：`scripts/check-yaml-doc.mjs` 的 `kind-parity` 把
`web/yamldoc.js` 的 `KINDS`（`node:vm` 载入）与 `internal/server` 的文档结构体
（`nodeYAMLDoc` / `nodeYAMLMeta` / `nodeYAMLSpec` / `nodeYAMLLimits`、
`namespaceYAMLDoc` / `namespaceYAMLMeta` / `namespaceYAMLSpec`）的 `yaml:"…"` 标签
逐路径比对**同名字段集 + 相对顺序**。服务端加一个文档字段而前端没跟上 ⇒ 红；
两边顺序不同 ⇒ 红。

**刻意不做**的是逐字节相等：服务端 `yaml.v3` 对 `'*'` 用单引号、对 `/opt/go1.25` 裸写，前端更保守
一律加双引号（`"*"` / `"/opt/go1.25"`）。两者都是合法 YAML 且解析等价；读面写 `total: null`、
写面把它当「不设置」是同一语义的两种表示（§5），逐字节相等既不可能也不该是判据。

## 7. 变异自证（换回旧实现 → 保真判据必红）

| 变异 | 手段 | 结果 |
| --- | --- | --- |
| 换回改前页面那份拼装实现（`docToYAML` + 只有 4 个键的草稿） | `scripts/yamldoc.test.mjs:429`（同一组保真判据跑两遍） | findings 非空，`tags` / `extra` / `limits` 三个维度各有点名；统一层在同一组输入上 findings 为空 |
| 换回改前命名空间页那种「只有 vars」的拼法 | `scripts/yamldoc.test.mjs:440` | `tags` 维度必红（点名 `spec.tags`） |
| 页面里加回 `const yamlScalar = …` | `check-yaml-doc` | 红，点名 `dup-serializer` |
| 页面里拼 `'apiVersion: ' + 'anotify/v1'` | `check-yaml-doc` | 红，点名 `doc-literal` |
| 前端 kind 表删掉 `tags` | `check-yaml-doc` | 红，点名 `kind-parity`（并指出服务端结构体的行号） |

`check-yaml-doc --self-test` 用纯内存夹具做红/绿双侧自证（51 条），红用例断言命中的**规则码**，
绿用例断言扫描面计数大于零 —— 「绿」不许与「没检查」同形。

## 8. 复算命令

```console
$ node --test scripts/yamldoc.test.mjs          # 文档层：标量 / 保真 / 省略规则 / 变异 / 页面接线
$ node --test scripts/vars-table.test.mjs       # 表格行逻辑（序列化已搬走）
$ node scripts/check-yaml-doc.mjs --self-test    # 检查自身的红/绿双侧
$ node scripts/check-yaml-doc.mjs                # 当前仓库
$ make fe                                        # 生成 web/ 产物 + 指纹（sitegen / class / i18n / 文案）
$ make ci                                        # 全量静态门禁 + 单测 + 构建
$ make e2e-affected                              # 按改动选择（本次是映射表改动 ⇒ 全量）
```

## 9. 落地数字

- `make ci`：**exit 0**（`contract` → `check-agents` → `lint-go` → `fe` → 守卫自测 → `check-yaml-doc`
  → 测试纪律 → 注解 → 文档端点/样例 → 选择器 → 前端单测 → 秘密扫描 → 构建 → `go test ./...`）。
- `make e2e-affected`（本次因映射表改动按保守策略跑了全量）：**29/29 套件、2468 断言通过 /
  0 失败**（基线 2455，新增的 13 条是本轮的保真与边界断言）；判定器「清单内 0 / 清单外 0」。
  审查修复（§10）之后又整跑了一次全量，数字与判定完全一致。
- 棘轮：`unmarked 312 / 基线 312`、`unbacked 299 / 基线 299`（都没有上调）；
  测试纪律五项与基线一致。

## 10. 审查里修掉的两处（都在本 issue 的判据面上）

对抗审查（独立子节点，只读）在交付前找出两处「静默改事实」，都已修并有回归测试：

1. **块标量 chomping**（`web/yamldoc.js` 的 `blockScalar` / `parseBlockScalar`）：原来写死 `|`
   并把尾部换行全剥掉，读的一侧把 `|-` 当 `|`。含换行的值（证书 / JSON / 脚本片段）每保存一次
   就被加一个行尾换行。现在两侧都按 yaml.v3 的规则实现，并用「前端写 → yaml.v3 读」「yaml.v3 写
   → 前端读」双向对拍过 14 种值（含 `\r\n`、行内制表符、首行缩进、3 个行尾换行）。
2. **被压住的 vars 被表格保存删掉**（`web/vars-table.js` 的 `rowsToVars`）：`local`/`node` 压住
   本层同名变量时，那一行不属于本层文档（表格里显示的是生效值），旧的折回逻辑会把它从本层文档里
   去掉。现在 `rowsToVars(rows, base)` 以文档里现有的 `spec.vars` 为基线：没被用户碰过的键原样
   保留（用文档里的值，不是生效值），用户显式删掉才消失。这是本次之前就存在的缺陷（基线同形），
   但本轮把它作为唯一写路径重写，就一并修了。

## 11. 遗留风险与后续

- **`Capability` / `AnodeConfig` 两个 kind 目前没有页面写路径**（能力的编辑仍走 JSON `POST`，
  `anode.yaml` 的写入方是 install 脚本）。它们的价值是把「同一份文档的规则」收在一处，并让保真
  判据覆盖 `requires.values` / `run.workDir` / `envPassthrough`；将来能力改 YAML 写入口时不用再
  造第三份实现。
- **前端 `parse` 只认服务端读面的方言**。遇到拒绝集（折行块、锚点、多文档…）时它返回 problems，
  页面**拒绝改写**并提示「在文本框里直接改」——不猜、不兜底成空。服务端读面的文本永远在这个
  方言内，所以这条路径只在用户手贴奇怪文本时出现。
- **预览 mock 的 Node 文档少一行 `total: null`**（读面语义）。预览的文本与页面保存时生成的文本
  因此完全一致；真服务端的读面仍写 `total: null`（§5 的边界断言在真服务上跑）。
- **手改的草稿会被表格保存覆盖**：YAML 编辑区有未保存的手改时点表格保存，写出去的是「服务端原文
  + 表格改动」。改前是同一个行为（草稿在内存里、表格保存不读它），本次没有改变，也没有顺手加
  提示 —— 要加的话是独立的一小步（`yamlDirty` 已经在页面里，条件现成）。
- 前端单测 `scripts/v4-frontend.test.mjs` 有一批**既有**失败（rebase 后的基线 `1dbe9d70`：215 条
  用例里 43 条失败，它不在 `make ci` 的接线里）。本分支上仍是 215 条 / 43 条失败，而且**失败集合
  与基线逐条相同**（判据是集合相同，不是 0 fail）—— 没有新增失败。这批失败的根因（fixture 与 v4
  形状漂移）不在本 issue 范围内，本次没有顺手改它们。

## 12. rebase 到新 main 之后的调整（#495 注释棘轮）

PR 的树落后于 main：`#497` 把注释/文档密度冻结成棘轮（`make check-comment-density`），`#498`
又把 `#475` 的注释压回基线。rebase 只有一处真冲突（`Makefile` 的 `.PHONY` 与 `ci` 目标列表 ——
两边各加一个守卫，取并集：`check-comment-density` 与 `check-yaml-doc` 并存），但**注释预算**是
必须重做的部分：本 PR 带进来 405 行新注释，`scripts` / `scripts/e2e/suites` / `web` /
`web-src/pages` / `web-src/layouts` 同时超限。

处理方式是「只删注释、不抬基线、也不 `--update`」：删掉复述函数名的、解释显而易见分支的、
`/* ===== ① ===== */` 与 `// ---------- xxx ----------` 分隔线、`#473` 交付横幅与「本文件曾经…」
的流水账；只留**代码无法自述的外部约束**——`web/yamldoc.js` 头部 14 行（chomping 与 yaml.v3
对拍、JSON 双引号标量、数字开头必须加引号、`total: null` 整键省略、值节点整份替换、读不懂的
方言不得渲染）、`scripts/check-yaml-doc.mjs` 头部 2 行（唯一序列化实现 + kind 表同序）、页面里
「表单编辑的是同一份文档」那一段。每个目录因此回到**恰好**基线（`web` 4674、`scripts` 3655、
`scripts/e2e/suites` 2182、`web-src/pages` 2400、`web-src/layouts` 98；仓库总量 43411），
`unmarked 312` / `unbacked 299` 两项也未动。

本节以上各节的行号按 rebase 之后的位置重新校正过（代码语义与断言一条没改）；§9 的两个数字
（`make ci` exit 0、e2e 29/29 · 2468 断言）在 rebase 之后重跑得到的仍是同一组值。
