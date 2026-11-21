# docs · 文档入口

> 一句话：**行为的唯一权威是代码、测试与 `docs/contracts/`。**
> 文档只解释「为什么」和「怎么用」，本身不是行为的判据。

## 1. 权威顺序

| 层 | 载体 | 说明 |
| --- | --- | --- |
| ① | **代码 + 测试** | 真实行为：读写语义、派生状态、校验规则、错误码 |
| ② | **`api/` 五份 OpenAPI** | HTTP 形状（path / 字段 / 信封）的正本 |
| ③ | **`docs/contracts/`** | 冻结契约：不变量、wire 字节、裁决记录、决策索引 |
| ④ | 本目录其余文档 | 面向使用者的操作与排障说明，**不是**判据 |

冲突时按上表从上往下读。代码与契约打架 ⇒ 先改代码、再同步契约（见 §4）。

## 2. 设计文档已移除（2026-11-19）

`docs/` 下的设计文档与仓库根的静态设计稿目录已**整套删除**。

理由：那些文档是**实施前的计划**，而计划已经全部落地。留着它们只会让「文档说 A、代码做 B」
变成常态——其中一份文件头的状态至今还写着「语义冻结；实现未开始」，而代码早已跑在生产上。
一个没有执行者的叙述会随时间只能变错，而读的人对它的信任度高于对代码的信任度。

要看历史：

```bash
git log --diff-filter=D --name-only -- docs/ | grep -i design   # 被删掉的那些文件
git show <sha>:<path>                                          # 具体某一份当时的内容
```

决策号（`V4-D1`～`V4-D6`）没有随之消失：索引的**代码侧落点**是 `internal/decisions/decisions.go`
（编号 → 执行者，由该包的测试核对），[`contracts/v4/CONTRACT.md`](contracts/v4/CONTRACT.md) 的 §0 是同一份索引的契约副本。

**历史别名**：代码与 `api/` 的描述里仍有 `api-conventions §4.1`、`capability.md §2` 这类
**按章节号的短名引用**——它们指的是已移除设计文档里的章节。章节号是历史坐标，不是可点的链接；
现行事实在 `api/` 的 schema 与代码里。要看「当时写了什么」，用上面的 `git log` 命令取原文。

## 3. 面向使用者的文档

| 文档 | 读者 | 用途 |
| --- | --- | --- |
| [`../README.md`](../README.md) | 使用者 | 产品定位、快速开始 |
| [`agent-install.md`](agent-install.md) · [`anode-install.md`](anode-install.md) | 运维 | 节点/agent 的安装、自升级、退出码与排查 |
| [`dev-docker.md`](dev-docker.md) | 开发者 | 用容器起一套本地服务 |
| [`lint-guards.md`](lint-guards.md) | 贡献者 | 静态检查总账：哪条纪律由哪个检查负责 |
| [`contracts/`](contracts/) | 贡献者 | 冻结契约与不变量（**规范性**） |
| [`reports/`](reports/) | 排障者 | 事故与交付的历史取证记录，**只读快照** |
| [`history/`](history/) | 排障者 | 更早的档案与已迁移的协调者台账 |

`reports/` 与 `history/` 里出现的数字、路径、行号都是**当时的定格**，不要当现行契约读。
它们的价值是回答「当时为什么会这么做」，不是回答「现在是什么」。

## 4. 改行为的规矩

1. **先改代码与测试**：新增或修改行为时，先落实现与断言。
2. **再同步契约**：把行为写进 `docs/contracts/` 的对应条目（不变量、wire 字段、决策号）。
   `@decision` 锚点解析到决策索引：`internal/decisions/decisions.go` 与 `docs/contracts/`
   （探测顺序见 `scripts/check-annotations.mjs`）。
3. **不要写「愿景文档」**：没有执行者的计划不入库。要做的事进 issue/PR；已经定的事进契约并挂锚点。
   落笔前问一句：**这段话有没有执行者？** 没有就不要写。
4. **文档里的路径 / 命令 / 端点都必须是可解析的外键**：`make ci` 会跑
   `check-annotations`（引用与锚点）、`check-doc-endpoints`（端点）、`check-doc-links`
   （协调者工作区引用），写错的路径当场红。

## 5. 注释与文档只留「代码无法自述」的部分（#495）

- **注释**只写：非显然的取舍、外部约束（内核/浏览器/协议怪癖）、危险操作警告。复述函数名、
  解释显而易见的 `if`、历史流水账、TODO 式吐槽 —— 一律删。
- 两道棘轮各管一段：`check-annotations`（L5）管**留下的**注释不许说谎（引用/锚点必须解析）；
  `check-comment-density`（L12）管**总量**只许降 —— 目录与仓库注释行冻结在基线，
  单文件上限 = max(12, 代码行 × 0.5)。
- **文档**只留操作手册。`docs/design`、`docs/specs`、`docs/plans` 重建即判红；
  `docs/v4`、`docs/research`、`docs/perf` 已删（重建即判红）；`docs/contracts` 的文件数进了棘轮，只许下调。
  容量与背压的测量快照留在 [`reports/capacity-and-backpressure.md`](reports/capacity-and-backpressure.md)。
- 删完同批下调基线：`node scripts/check-comment-density.mjs --update`（只降不升；抬高直接拒）。
- 逐目录判据与分批计划见 [reports/comment-and-doc-ratchet.md](reports/comment-and-doc-ratchet.md)。
