> **历史快照**：本文件是 2026-11-27 的定点核对记录，不随 issue 演进；状态以 GitHub 上 `audit-2026-11` 标签为准。

snapshot-doc: 2026-11-27 审计总账 #648 的落地台账（复核命令见 §5）

# 审计总账（#648）· 落地台账

## 1. 这份文件管什么（以及不管什么）

81 条清单**只**存在于 issue 本体（分组清单 A–F、优先级分布、同根关系）。本文件不复制清单，
只存三件 issue 装不下的东西：

1. **状态定格**：哪条落地了、由哪个 PR 落地。issue 的复选框只有开/关，
   不看 PR 就分不清「修好了」与「关掉了」。
2. **口径**：**关闭 ≠ 落地**。本仓已经出现「关闭但代码没改」的实例（§3.2），
   triage 不能只看 state —— 这正是本仓 lint 总账 §0 的缝隙③「静默豁免」的同形。
3. **复核命令**：§5 跑一遍即可刷新整份台账，不需要人肉对 81 条。

不把 81 行清单搬进仓库的理由：清单与 issue 会各自演化（`docs/lint-guards.md` §0 的缝隙⑤
「同一事实两处各自演化」）。清单本体留 issue，仓库只留定格与复核方式。

## 2. 定格（核对日 2026-11-27）

| 组 | 条数 | 开 | 关 | high | medium | low |
| --- | --- | --- | --- | --- | --- | --- |
| A 访问控制与身份 | 14 | 14 | 0 | 7 | 6 | 1 |
| B 节点「在线但不干活」 | 8 | 8 | 0 | 3 | 3 | 2 |
| C 配置与数据被静默丢掉、或写了不生效 | 16 | 9 | 7 | 5 | 8 | 3 |
| D 核心卖点与新用户第一条路径 | 8 | 7 | 1 | 4 | 4 | 0 |
| E 自动化搭建与排障 | 15 | 12 | 3 | 4 | 8 | 3 |
| F 全站体验：信息架构、一致性、可访问性、多语言 | 20 | 16 | 4 | 1 | 14 | 5 |
| **合计** | **81** | **66** | **15** | **24** | **43** | **14** |

**优先级分布与 issue 标签逐项一致**（24 / 43 / 14），但**口径必须先排除 epic #648 本身**：
epic 自己就带 `audit-2026-11` + `priority/high`，不排除会得到 82 / 67 / 15 / high=25。
排除 #648 后三者相等 —— A–F 六节的 emoji 计数、`priority/*` 标签计数、本表末行（81 = 82 − 1）。
复算见 §5 ①（命令里已写死 `select(.number != 648)`）。

需要分清两件事：§5 ① 复算的是**总数与优先级**；A–F 的**组归属**不在任何命令里 ——
它只存在于 issue body 的 6 个小节（§1：清单不进仓库），所以「每一格都来自这一条」只对
条数/开闭/优先级成立，对分组不成立。

## 3. 已关闭的 15 条

### 3.1 有落地 PR 的 13 条

| issue | 落地 PR | 关闭日 | 组 | 一句话 |
| --- | --- | --- | --- | --- |
| #610 | #672 | 2026-11-26 | C | cron 日/周字段的 `*/N` 步长不再被丢 |
| #616 | #678 | 2026-11-26 | D | Key 页「生成」防重，双击不再多建一把 |
| #633 | #684 | 2026-11-27 | E | poll 退避以计划间隔为基准 |
| #634 | #671 | 2026-11-26 | E | `emit --data` 非 JSON 对象报 param_type |
| #635 | #673 | 2026-11-26 | C | 预案包内 YAML 多文档不再静默丢弃 |
| #637 | #676 | 2026-11-26 | C | poll 源原始配置读失败时锁死保存 |
| #638 | #686 | 2026-11-27 | E | 事件流每行带 trace 级派发结果 |
| #639 | #679 | 2026-11-26 | F | 页头刷新不再覆盖未保存的 vars 草稿 |
| #642 | #664 | 2026-11-26 | C | `spec.values` 的 schema 自身在 Parse 期校验 |
| #644 | #658 | 2026-11-26 | C | spec 小矛盾（version 下界 / timeoutMs 上限 / 错误前缀） |
| #645 | #677 | 2026-11-26 | F | 保存失败把 `details.causes` 标到对应字段 |
| #646 | #662 | 2026-11-27 | F | yamldoc 裸写冒号结尾标量 + 超 2^53 整数 |
| #647 | #665 | 2026-11-26 | F | 保留期保存先确认，并给逐字段影响摘要 |

其中 **#633 / #638 / #646 三条的 `closedByPullRequestsReferences` 为空**：落地 PR
（#684 / #686 / #662）只在标题或正文里写 `#633` / `修 #638` / `(#646)`，没有 closing keyword，
GitHub 因此不建立引用 —— 代码确实在 main 上（例：#646 的 `web/yamldoc.js` 的 `Number.isSafeInteger` 处理）。
这三行的 PR 号来自**标题检索**（§5 ② 的「补充取证」），不要因为判据 ① 为空就把它们打回「关而未修」。

### 3.2 关闭但**没有**落地 PR 引用的 2 条（需要复核）

| issue | 关闭日 | 代码复核（2026-11-27） |
| --- | --- | --- |
| #640 | 2026-11-26 | **仍未修**。`internal/store/playbook_tx.go:353` / `:356` 仍把 `*artifact.Error` 压成字符串（`%w: %s`）；`internal/server/api_status.go:203` 落 400 时传的是**通用常量** `reasonBadRequest`（`internal/server/api_status.go:23`，值为 `"BadRequest"`，不是 `bad_request`）且 `causes=nil`；而同仓直接创建能力走的是 `capabilitySpecError`（`internal/server/resource_capability.go:1592`，有封闭 reason + causes）。issue 指出的两条入口错误质量不对等原样成立。 |
| #643 | 2026-11-26 | **部分**（issue 的 3 项逐条，别只读结论）：① `items` 的**布尔**写法已补且真被执行（`internal/schema/schema.go:313-316`；`go test ./internal/schema/ -run TestValueRejectsByFalseChildSchemas` 的 `items false 拒绝任何元素` / `prefixItems 之后的 items false` 均 PASS）—— 但 **draft-07 的元组 `items: [...]` 仍被静默忽略**：`schema.go:314-315` 直接 `hasItems = false`，`validate.go:114-120` 是 `continue`（注释「不在受控子集内：忽略」），**元组不是已约束**；② 错误文案硬编码 `params_schema` 仍在（`schema.go:422`）；③ **「只报第一条错误」未修**：`schema.go:248` 仍直接 `return … missing required field …`，包内只有 `Value`，没有 `Values` 类多错 API（`grep '^func ' internal/schema/schema.go` 可验）。 |

判据是下面三条事实的**交集**（三条都成立才判「关而未修」；写成并集会把 §3.1 里已落地的
#633 / #638 / #646 判成需复核 —— 它们恰好只满足 ①）：

1. `closedByPullRequestsReferences` 为空（§5 ②）；
2. 按 **PR 标题/正文**检索也找不到修复该 issue 的 PR（`gh pr list --search '#<n> in:title'`）；
3. 代码复核仍能重现 issue 描述的行为（`git log -S` 查内容或直接读文件，§5 ③）。

**这两条不该被算成「已覆盖」**：下一轮要么按上面的复核结论重开、要么补修后回填 PR 号。

为什么判据 3 不写成「`git log -- internal/schema/` 里没有对应修复提交」：#643 第 ① 项的布尔修复
是从 #642 的 PR #664（`b6a9e01e`）进来的 —— 目录里**有**这个提交，但 #643 的第 ②③ 项仍未修。
「某目录有/没提交」既不充分也不必要（修复也可能落在别的包），只有读代码能判。

## 4. 「建议修复顺序」的现状

issue 的「第 0～3 步」写于 2026-11-24，此后 13 条真正落地、2 条关而未修，顺序需要重排：

- **第 0 步（#574 注册闸门 / #577 产品决策）**：两条都还开着，仍是阻塞位；A 组 14 条 0 落地。
- **第 1 步 B 组四连 #567 → #569 → #590 → #617**：**4 条全开**，整组 8 条 0 落地 —— 仍是止血位，
  优先顺序不变。
- **第 2 步 C 组数据保真**：C 组关了 7 条，其中 #640 / #643 是「关而未修」（§3.2），
  实际落地 5 条；剩余 9 条里 #568 / #583 仍是最伤的（同一份源配置的三条路径）。
- **第 3 步 F 组共享 helper**：F 组已关 4 条（#639 #645 #646 #647），但都是页面级修补，
  4 个共享 helper 本身还没开始抽。

## 5. 复核（怎么刷新这份台账）

```console
# ① 状态与优先级：§2 的条数/开闭/优先级都来自这一条。
#    口径：排除 epic #648 自己（它带 audit-2026-11 + priority/high；不排除会得到 82 / 67 / 15 / high=25）。
$ gh issue list --repo zzjcool/anotify --label audit-2026-11 --state all --limit 200 \
    --json number,state,labels \
  | jq -r 'map(select(.number != 648))
      | "total=\(length) open=\(map(select(.state=="OPEN"))|length)"
        + " closed=\(map(select(.state=="CLOSED"))|length)"
        + " " + ([.[] | .labels[].name | select(startswith("priority/"))] | sort
                 | group_by(.) | map("\(.[0])=\(length)") | join(" "))'
# → total=81 open=66 closed=15 priority/high=24 priority/low=14 priority/medium=43
#    分组（A–F）不在这条命令里：组归属只存在于 issue body 的 6 个小节。

# ② 落地 PR：§3.1 的 13 行里 10 行由这一条给出（对每条已关闭的 issue 跑）
$ gh issue view <n> --repo zzjcool/anotify --json closedByPullRequestsReferences
#    批量：
$ for n in 610 616 633 634 635 637 638 639 642 644 645 646 647; do
    printf '#%s ' "$n"
    gh issue view "$n" --repo zzjcool/anotify --json closedByPullRequestsReferences \
      --jq '[.closedByPullRequestsReferences[].number] | join(",")'
  done

# ②补充取证：判据 ① 为空、但 PR 标题里写了 #n（#633 / #638 / #646 就走这条）
$ gh pr list --repo zzjcool/anotify --state all --search '#<n> in:title' --json number,title

# ③ 关而未修的复核：只有「读代码」能判，别用 state 代替。
#    ⚠ 不要用 `git log --all --grep='#<n>'` 当判据：本文件的提交消息里就写着 #640 / #643，
#    合并到 main 后任何人 grep 都会命中本文件 —— 假阳性，正好把「关而未修」判成「已落地」；
#    反方向也漏：只在 PR 标题写 #n 的落地提交，提交消息里 grep 为 0（假阴性）。
#    要证明「代码里仍是老行为」，用内容检索 + 读文件 + 跑测试：
$ git log origin/main -S '拒绝任何元素' --oneline -- internal/schema/
$ sed -n '313,316p' internal/schema/schema.go   # items 布尔已执行 / 元组仍 hasItems=false
$ go test ./internal/schema/ -run TestValueRejectsByFalseChildSchemas -v
```

## 6. 已知偏差（本轮一并修掉 / 明确不修的）

1. issue 本体的 81 个复选框在 15 条关闭后仍未勾选 —— 本轮按 §3 同步勾选，并在 body 里指向本文件。
   issue body 不做全量状态维护（那是本文件的职责），只保证「勾选 = 真的落地」。
2. body「顺带修掉的文档/工具问题」里那条 *`internal/store/AGENTS.md` 说迁移引擎只支持单块，
   但 `headVersion=11`* 已经过期：该文件现在写的是分块迁移 + `headVersion` 必须等于块数
   （`internal/store/AGENTS.md:22`，常量在 `internal/store/migrate.go:35`）。本轮把 body 里这条改成「已修」。
3. **复核命令的自证（review 第 1 轮打回点）**：§5 ① 原来跑出来是 82 / 67 / 15 / 25，§2 写的是
   81 / 66 / 15 / 24（epic 未排除）；§3.2 判据原来写「并集」且用 `git log --grep='#<n>'`，
   已被本 PR 自己的提交消息证伪。本轮把 ① 的口径写进命令、判据改交集、③ 去掉 grep 改为
   内容检索 + 读文件，并把 #633 / #638 / #646 三条「引用缺失但已落地」列为已知例外。
4. body 引用的 `reports/r3-web.md` / `reports/r3-backend.md` / `reports/c1-data.md` **不在仓库里**
   （`git ls-files` 无匹配）。所以「覆盖缺口」那节的 W2/W4/W5/W6/W9/W10/W12/W14、B9、S4–S7
   无法在仓库内复核 —— 它们是**未验证**，不是「已验证为假」。本轮把这一点写进 body。
