> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 2026-11-19 设计文档移除时的定格数字（复算命令见 §5），不是「现在是多少」的承诺 -->
# 移除设计文档，代码成为唯一权威（Refs #473）

用户决定（原话）：

> 机制文档都是过期的，项目里面的设计文档都可以移除了，我们以后就是以代码为准，所有的设计文档都已经实现了。

一句话：**删掉「实施前的计划」，把它们的语义落点改成代码 / `api/` / `docs/contracts/`**。
删除比例子更麻烦的是「引用清理」与「守卫对齐」——设计文档同时是 `@decision` 的解析落点和
若干静默排除项，直接删会让检查变红或（更糟）变假绿。

## 1. 删了什么

| 目录 | 文件数 | 内容 |
| --- | --- | --- |
| `docs/` 下的设计文档 | 18 | `v4/`（README / decisions / capability / source / anode / playbook / config / api-conventions）+ 十份主题设计（namespaces / nats-transport / emit-protocol / capability-spec-v3 / typed-pipeline / capability-lifecycle / node-namespace-ia / agent-upgrade / playbooks / param-mapping） |
| 仓库根的静态设计稿 | 43 | `mockup-*.html`、`tech-scheme.html`、`edge-node-flow.html`、`flowline-lab.html`、`font-compare.html`、`trace-visual-lab.html`、`tokens.{css,js}`、`ui-v2/`（视觉稿 + 偏离评审 + 自检脚本）、`wiki/`（十页设计 wiki） |
| **合计** | **61** | 全部是「计划/样稿」，没有一份被构建、embed 或运行时读取 |

保留（**按任务书，不动**）：`docs/contracts/`（规范性契约，有测试钉住）、`docs/*-install.md`、
`docs/dev-docker.md`、`docs/lint-guards.md`、`docs/v4/`（工具链手册）、`docs/history/`、`docs/research/`、`docs/perf/`。
`docs/reports/` 保留但每份顶部加了「历史快照」banner（17 份），**内容不改写**。

## 2. 引用清理：129 处 / 68 个文件

复算（基线 `141edd25`，即本次工作开始时的 `origin/main`）：

```bash
# 被删的设计目录名（拆开写，免得这份报告自己命中下面那条断言）
D="docs"$'/'design
git grep -o "$D" 141edd25                                       | wc -l   # 89
git grep -o -P "(?<!docs/)design/" 141edd25 -- . ':!design' ":!$D" | wc -l   # 40
```

> 期间并行合入的 #474 又在它自己的交付报告里新引了 3 处同一个路径（`origin/main` 上现为 92），
> 同一批一并清掉了 —— 所以「归零」是相对**当前 HEAD** 的断言，不是相对某个基线快照。

替换原则（按引用类型分三类，**不留死链，也不新增「指向空气」的锚点**）：

1. **代码注释**（`internal/`、`cmd/`、`web/`、`web-src/`、`scripts/`）→ 指向**实现位置**
   （`file:func/line` 或结构体名）。例：
   - `internal/server/resource_node_yaml.go` 的「IA 重构 ②，<设计文档> §2.1/§3」→ 去掉文档号，
     边界改为对照 `api/resource.yaml` 的 Node 文档形状；
   - `internal/sourcespec/sourcespec.go` 的「语义基线是 <设计文档>」→「语义基线是本包的校验函数与 `api/ingress.yaml`」；
   - `internal/playbook/sensitive.go` 的「契约口径见 <设计文档> §1.2」→「契约口径就是本文件 `maskUnprovableKeywords` 的 allowlist 语义」；
   - `Dockerfile.anode`、`docs/v4/anode-cli.md` → `cmd/anode/internal/config/config.go` 的 `Config`。
2. **契约文档里的「设计依据」**（`docs/contracts/**`）→「实现见 `file`」。`docs/contracts/v4/CONTRACT.md`
   新增 **§0 V4 已批准决策索引**：`V4-D1`～`V4-D6` 每条一行「决定了什么 + 实现在哪」，
   因为 `@decision` 锚点的原落点（设计文档）被删了，决策号必须有新家（见 §3）。
3. **历史记录**（`docs/history/`、`docs/research/`、`docs/reports/`）→ 改成「当时的…（已随设计文档移除）」，
   指路统一到 `docs/README.md` 的历史入口，不重写内容。

**新增 `docs/README.md`**：权威顺序（代码+测试 > `api/` > `docs/contracts/` > 其余文档）、
设计文档为何移除、面向使用者文档的分工（install / lint-guards / contracts / reports / history）、
以及「改行为的规矩」——先改代码与测试 → 再同步契约 → **不要写愿景文档**（判据：这段话有没有执行者）。

### 2.1 有意不扩面的部分（已写进 `docs/README.md`）

全仓还有 475 处（148 个文件）`api-conventions §4.1` / `capability.md §2` / `source.md §5` 这种
**「短名 + 章节号」引用**（Go 注释、`api/*.yaml` 的 description、e2e 夹具里）。本次**不动**：
任务书的清理面是「`docs/` 下的设计子目录」与「仓库根的设计稿目录」两个**路径**形态，共 129 处；上述短名引用不是路径，
而且大部分落在 `api/` 五份**真源**的散文里——一起改会把一个「删文档」的 PR 变成一个
「重写接口描述」的 PR，风险大于收益。处理方式：`docs/README.md` §2 明说它们是**历史坐标**，
要原文就用 `git log` 取。若需要一并对齐，另开一条。

## 3. 守卫对齐（4 个脚本 + 1 张映射表，**没有下调任何下限**）

| 位置 | 原来依赖被删文件的地方 | 改成 | 是否动了下限 |
| --- | --- | --- | --- |
| `scripts/check-annotations.mjs` | `DECISION_DOC_CANDIDATES` ①=设计文档 `decisions.md`、③=设计目录下 `v4/*.md` 的 glob | ①=`docs/contracts/v4/CONTRACT.md`、②=`docs/contracts/CONTRACT.md`；删掉 glob 探测与 `DECISION_DOC_GLOB_DIR` | 否（`ANCHOR_MIN=4` 等四条下限原样） |
| 同上 | `EXCLUDE_PREFIXES` 里的根 `design/`、注释里「`docs/` 下的设计目录必须扫」 | 删掉该排除项与过时说明 | 否 |
| 同上 | `KNOWN_FACT_PROBES` 里指向被删文件的探针 | 换成仍存在的真实历史提及 | 否 |
| 同上 | 自测 ㉒「decisions.md 缺时退到 CONTRACT.md」 | 「设计文档已移除，决策号由契约文档提供」 | 否 |
| `scripts/e2e/affected-map.mjs` + `affected.test.mjs` | `NO_E2E_IMPACT` 里的 `design/**`（表腐烂守卫要求每条 glob 至少匹配一个被跟踪文件） | 删掉该行（目录不存在了） | 否 |
| `scripts/check-test-discipline.mjs` | `EXCLUDED_SEGMENTS` 的 `/^design$/` | 删掉 | 否 |
| `scripts/check-doc-endpoints.mjs` | 扫面说明里的 `design/` | 删掉该词 | 否 |
| `scripts/check-ui-copy.mjs` | 自测红样例用设计目录下的路径做 `docs-path` 命中串 | 换成仍存在的契约路径（同一条规则照测） | 否 |
| `.gitignore` | `design/silk-shots/` 截图忽略项 | 删掉 | — |

**棘轮校准（唯一动过的数字）**：`COUNT_UNMARKED_BASELINE` 从 385 下调到实测值 —— 下降来源
100% 是「`docs/` 少了 18 份 markdown 散文」，不是放宽判据。这是棘轮语义要求的动作
（低于基线也红 = 强迫同批下调，免得过期基线变成下一次回归的挡箭牌）。
`CLAIM_UNBACKED_BASELINE` **未动**（原基点上实测仍是 300，零余量保持；rebase 到 #479 之后
main 已把它收到 299，见 §6），本轮新增的注释一句都没带「有检查在兜底」这类声称。

## 4. 三条自证

**① 设计文档路径引用归零**

```
$ D="docs"$'/'design; git grep -c "$D"; echo "exit=$?"
exit=1
```

**② 两棵目录真的没了**

```
$ D="docs"$'/'design; find "$D" design -maxdepth 1 2>/dev/null | wc -l
0
```

**③ 死链会被 `check-doc-links` 抓住（变异自证）**

在 `docs/README.md` 末尾故意加一条指向协调者工作区的悬空引用（先 `git add`，因为判据是 `git ls-files` 而不是工作区）：

```
$ printf '\n> 历史入口：`.pi-orchestrator/docs/DESIGN-INDEX.md`\n' >> docs/README.md && git add docs/README.md   # doc-links-ignore: 变异自证样例，见下
$ node scripts/check-doc-links.mjs; echo "exit=$?"
  扫描：817 个被跟踪文件（判据是 git ls-files，不是工作区）
  REF_OK  0 / REF_HIST  1 / REF_MIGRATED 32 / REF_LOCAL 49 / REF_DEAD  1
  ── REF_DEAD（引用指向 fresh clone 里不存在的文件）──
    docs/README.md:65  .pi-orchestrator/…（完整串同上一行）
  ❌ 死链：把目标迁到 docs/ 并改引用，或改成不指向具体路径的历史说明
exit=1
```

删掉那行后：

```
$ node scripts/check-doc-links.mjs; echo "exit=$?"
  扫描：817 个被跟踪文件（判据是 git ls-files，不是工作区）
  REF_OK  0 / REF_HIST  1 / REF_MIGRATED 32 / REF_LOCAL 49 / REF_DEAD  0
  ✅ REF_DEAD 0：仓库里没有指向协调者工作区的悬空引用
exit=0
```

## 5. 门禁

```
$ flock /tmp/anotify-ci.lock -c "make ci"; echo "EXIT=$?"
EXIT=0
```

被删的设计文档里有多份在文件头自称「语义冻结；实现未开始」——它们对应的行为早就跑在生产上。
这就是本次移除的核心理由：**没有执行者的叙述只能随时间变错，而读者对它的信任度高于对代码的信任度。**

## 6. rebase 记录（到 `57298070`，#490/#492/#484/#494/#479 之后）

本报告 §1～§5 的数字定格在工作分支的基点（`141edd25` 起算）。合入前 rebase 到 `57298070`
（已含 #490/#492/#484/#494/#479）时有两处冲突，处置如下（**产品行为零变化**）：

| 文件 | 为什么会撞 | 处置 |
| --- | --- | --- |
| `internal/server/resource_namespace_yaml.go` | #476 新增了 `PUT /v1/namespaces/{name}/yaml` 并改写了文件头；本 PR 要删文件头里的设计文档路径 | 保留 #476 的实现与语义（YAML 写入口、`metadata.resourceVersion` = 命名空间 vars 的 CAS 令牌，与 `GET …/config` 同一个值），只删掉那一行里的设计文档路径（`…-ia.md §2.2 / §3.1` 那半句）。与 `57298070` 的 diff 只剩这一行注释，没有代码行 |
| `scripts/check-annotations.mjs` | #479 已把棘轮从 385/300 收到 383/299；本 PR 要按删除设计文档后的实测值下调 | 取并集：`CLAIM_UNBACKED_BASELINE` = **299**（#479 实测，本 PR 未上调），`COUNT_UNMARKED_BASELINE` = **312** |

**为什么是 312 而不是本报告正文里的 314**：314 = 「385 - 71」，71 是删掉的十八份 markdown
在规则 D 扫描面里的贡献。并集树上 #479 已经先把 385 收到 383，同一份删除就是「383 - 71 = 312」。
实测复核（每棵树跑 `node scripts/check-annotations.mjs`，读它打印的实测值）：

| 树 | unbacked 实测/基线 | unmarked 实测/基线 |
| --- | --- | --- |
| `8dacf8c6`（本 PR 原基点） | 300 / 300 | 385 / 385 |
| `e9878432`（本 PR 原分支） | 300 / 300 | 314 / 314 |
| `57298070`（rebase 基点） | 299 / 299 | 383 / 383 |
| rebase 结果（本树） | 299 / 299 | 312 / 312 |

三个基点上的差量都是 71，与删掉的 markdown 数量对齐 —— 下降全部来自「整篇落在规则 D 扫描面里的
散文消失了」，不是判据放宽。也**没有**出现「实测 > 314、多出来的部分是文档被删导致注解失去锚点」
那种需要额外清注释的情况：`--report` 里 REF_UNRESOLVED / ANCHOR_UNRESOLVED 都是 0
（历史提及 29 条，同行/上一行已声明是退役物，不判红）。

**并集带进来的新引用**：#479 之后 main 上的 `docs/reports/vars-visibility.md`（#476）仍引着
已删的 IA 设计文档（路径就是 §2 里 `D` 变量拼出来的那种形态，`node-namespace-ia.md`）。按 §2 第 3 类（历史记录）改写为「当时的 IA 设计文档
（已随设计文档移除）」，权威口径指到 `internal/server/resource_namespace_yaml.go` 文件头 ——
所以 §4 自证 ① 在并集树上仍然成立。

### 6.1 变异自证：棘轮没有变成装饰品

把 `COUNT_UNMARKED_BASELINE` 改回 383（即并集前的旧基线）再跑：

```
$ sed -i 's/COUNT_UNMARKED_BASELINE = 312/COUNT_UNMARKED_BASELINE = 383/' scripts/check-annotations.mjs
$ node scripts/check-annotations.mjs; echo "EXIT=$?"
  unbacked 299 / 基线 299（约束性注释行共 300）
  unmarked 312 / 基线 383（计数型注释行共 317）
  [COUNT_UNMARKED_UNDER] 棘轮已改善但基线未下调（规则 D）（1 条）
    （棘轮）  COUNT_UNMARKED_UNDER  未标记的计数型注释 312 < 基线 383
      ✎ 把 COUNT_UNMARKED_BASELINE 下调到 312 再提交 —— 基线过期会成为下一次回归的挡箭牌。
EXIT=1
```

改回 312 之后 `EXIT=0`。这条变异证明的是「下调是真的被守卫要求的」：基线写松（留着 383）会红，
写死成实测值不够紧也会红（UNDER 要求同步下调），两头都堵住。

### 6.2 并集树上的门禁（真实退出码，不走管道）

```
$ flock /tmp/anotify-ci.lock -c "make ci"; echo "EXIT=$?"
  ✅ 模块约定与代码一致                                   # check-agents
  unbacked 299 / 基线 299（约束性注释行共 300）
  unmarked 312 / 基线 312（计数型注释行共 317）
  ✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。
  ✅ REF_DEAD 0：仓库里没有指向协调者工作区的悬空引用      # check-doc-links
  ✅ 文档端点检查：109 条 spec 路径 · 31 个文档文件 · 99 处 /v1/ 引用 全部存在于 api/*.yaml
  ✅ 五项全部与基线一致（t.Skip 登记 3/3 命中）            # check-test-discipline
EXIT=0

$ node scripts/e2e/affected.mjs --run-e2e          # = make e2e-affected
  ⚠️ 全量 e2e（29/29 套）：cmd/devseed/demo.go 落在共享面（每个套件都用 devseed 播种身份/Key）
  总耗时: 352.0s
  断言通过: 2455
  断言失败: 0
  ✅ E2E 全绿：29/29 个请求套件均达标（断言 2455 通过 / 0 失败，authority 清单覆盖 29 个）
EXIT=0

$ node scripts/e2e/ci-e2e-verdict.mjs --results .e2e-bin/results --known scripts/e2e/known-env-failures.json
✅ e2e 判定：清单内 0 条（已容忍），清单外 0 条
```

`make e2e-affected` 这次按「`cmd/devseed/demo.go` 落在共享面」选了全量 29/29 套。
对删文档的 PR 来说这是合适的口径：清单 `known-env-failures.json` 内 0 条、清单外 0 条，
**没有**用清单容忍任何一条失败。
