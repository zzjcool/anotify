# 机器路径回归节点变量：`repoDir` 从安装 values 里出去

> snapshot-doc: 2026-11-23 一次验证运行的原始输出定格（文中的数字是那一刻的实测值，
> 不是「现在是多少」的承诺；要可重跑的判据请跑文末那些命令）

## 问题

`examples/playbooks/issue-fix` 把仓库检出目录做成**安装参数** `values.repoDir`。

**为什么两台机器路径不同就非改不可**：安装 values 是**一份**，装出来的那批能力被**所有**
节点共用，所以 `values.repoDir` 只能是一个值——而实际两台节点的路径本来就不同：

| 节点 | 服务用户 | 仓库检出目录 |
| --- | --- | --- |
| `hw` | 普通用户（home `/home/zzjcool`） | `/home/zzjcool/code/anotify-agent` |
| `host-ws` | `root`（home `/root`） | `/root/code/anotify-agent` |

一个值对两个不同路径，只有两种结局：要么把两台机器的目录**抄成一模一样**（换用户、换
home、换机器就崩，而且这是人的纪律，不是系统的约束），要么其中一台在别人的目录里
`cd` 失败 / 跑错仓库。**"每台机器的目录"是机器事实，不是应用配置**——凡是把机器事实放进
安装 values，多节点就必然退化成"靠人把路径对齐"。这就是必须走节点变量的根本原因：
节点变量是**按 (node, ns) 逐键解析**的（local > node > namespace），一份定义、每台各自的
值，中枢在派发期就知道"这台机器给不出这个值"。

## 改了什么

| 文件 | 改动 |
| --- | --- |
| `examples/playbooks/issue-fix/capabilities/{triage,implement,review,fix}.yaml` | `run.env.REPO_DIR` 从字面值 `{{values.repoDir}}` 改成 `valueFrom.fieldRef.fieldPath: vars.REPO_DIR`；`spec.requires.values: [vars.REPO_DIR]`；脚本里的 `${REPO_DIR:?...}` 报错改成"该节点没配节点变量 REPO_DIR" |
| `examples/playbooks/issue-fix/playbook.yaml` | `spec.values` 去掉 `repoDir`（含 `required`）；`repo` 的 description 指向节点变量 |
| `examples/playbooks/issue-fix/values.demo.json` | 去掉 `repoDir`（demo 里不再有机器路径） |
| `examples/playbooks/issue-fix/README.md` | 新增「每台节点自己配 `REPO_DIR`」：两个**不同**的真实路径、PATCH 命令、缺配语义 |
| `examples/playbooks/issue-solve/README.md` | 一句结论：values 里没有绝对机器路径；唯一机器相关的是可选 `ghTokenFile`（默认空 = 匿名 API），要用就按同一口径改节点变量 |
| `examples/playbooks/pr-triage-notify/README.md` | 一句结论：`project/env/alertUrl/threshold` 与机器无关，不需要节点变量 |
| `internal/playbook/examples_test.go` | `TestExamplePackagesRender` 加守卫（见下） |

四个能力全改了（不是我最初以为的两个）：`triage/implement/review/fix` 都要 `cd` 进仓库，
漏一个就有一条能力在错的目录里跑。

脚本行为**没变**：仍然是默认临时目录 + `cd "$REPO_DIR"`，只是这个值的来源从 values 换成节点变量。

## 为什么这样最省

1. **没有新机制**：`requires.values` + `run.env.X.valueFrom` 是平台现成的两件套
   （`examples/capabilities/run-in-repo.yaml` 就是它的教学样例），解析在
   `internal/store/task.go` 的 `TaskInputs` / `config.go` 的 `EffectiveVars`，校验在
   `internal/artifact/artifact.go` 的 `validateEnv`。本轮一行生产代码都没改，改的全是示例与守卫。
2. **不加 `run.workDir`**：`run.files` 会展开进 workDir，`run.sh` 就会落进用户的 git 检出、
   参与 `git status`——而这条链的 agent 正是在这个仓库里提 PR。所以保留"默认目录 + 脚本
   自己 `cd`"，只换取值来源。目录**只有一个来源**，没有第二条路径可漂。
3. **不加默认值/兜底/探测**：缺配就走平台已有的失败路径（见下），不引入"两处都试一遍"的逻辑。

缺配时的失败语义（都是现成行为，不可绕过）：

- **派发期**：`spec.requires.values` 解析不出来 → `EligibleNodes` 把该节点判成
  `ValueMissing`，任务不派给它（`internal/store/node.go` 的 `nodeIneligibilityReason`）；
  所有节点都缺 → 没有可用节点（`NoMatchingNode` 条件），任务挂着等人补配置，**不会**退到某个默认目录。
- **执行期**：`run.env.REPO_DIR` 只来自中枢解析出的值，缺失就不会有 `$REPO_DIR`，
  脚本第一行 `${REPO_DIR:?该节点没配节点变量 REPO_DIR（spec.vars.REPO_DIR）}` 立即失败并说明原因。

## 守卫怎么咬人

`internal/playbook/examples_test.go` 的 `TestExamplePackagesRender` 里新增三条断言
（遍历所有 `examples/playbooks/*` 渲染出来的产物）：

1. `run.env` 的**字面值**不得以 `/home/`、`/Users/`、`/root/` 开头；
2. `run.workDir` 同理（同一个缺陷的另一半）；
3. 渲染结果再过一遍 `artifact.Validate`，断言 **0 problem**——
   "`vars.X` 引用必须在 `requires.values` 里"这一条归 `artifact`（`undeclared_need`），
   守卫只调用、不另写一套判定（两套判定迟早分叉）。

**先红后绿**（守卫先写，跑出失败；再改示例；同一测试转绿）：

```
$ go test ./internal/playbook -run '^TestExamplePackagesRender$' -count=1     # 改示例之前
--- FAIL: TestExamplePackagesRender (0.01s)
    examples_test.go:103: ../../examples/playbooks/issue-fix/anotify-triage 把机器路径 "/home/zzjcool/code/anotify-agent" 写进了 run.env.REPO_DIR 的字面值（命中前缀 "/home/"）：示例包装一次对所有节点生效，机器路径必须写成 valueFrom 引用节点变量
FAIL
FAIL	github.com/zzjcool/anotify/internal/playbook	0.439s
...
$ go test ./internal/playbook -run '^TestExamplePackagesRender$' -count=1     # 改完之后
ok  	github.com/zzjcool/anotify/internal/playbook	0.071s
```

**变异自证**（在最终版守卫上各做一次，做完还原）：

变异 A —— 把 `fix.yaml` 的 `valueFrom` 改回字面机器路径，**`requires.values` 保持不动**
（隔离"机器路径"这一条断言）：

```
--- FAIL: TestExamplePackagesRender (0.00s)
    examples_test.go:83: ../../examples/playbooks/issue-fix/anotify-fix 把机器路径 "/home/zzjcool/code/anotify-agent" 写进了 run.env.REPO_DIR 的字面值（命中前缀 "/home/"）：这个包装一次就对所有节点生效，机器路径必须写成 valueFrom 引用节点变量
FAIL	github.com/zzjcool/anotify/internal/playbook	0.117s
```

变异 B —— 保留 `valueFrom`，只删掉 `requires.values: [vars.REPO_DIR]` 那一行
（命中 `artifact` 的等价校验，说明没重复实现）：

```
--- FAIL: TestExamplePackagesRender (0.00s)
    examples_test.go:68: ../../examples/playbooks/issue-fix Render: invalid_playbook: capabilities/fix.yaml 渲染后不是合法能力文档：undeclared_need: run.env 的 REPO_DIR 引用了 vars.REPO_DIR，但没写进 spec.requires.values
FAIL	github.com/zzjcool/anotify/internal/playbook	0.089s
```

还原后同一个测试 `ok ... 0.112s`。两条断言各咬一种退化，都不是恒红。

## 运维步骤：每台节点各自设自己的路径

**变量名是 `REPO_DIR`，不是 `repoDir`**：`spec.vars` 的键必须匹配
`^[A-Z][A-Z0-9_]{0,63}$`（`internal/store/config.go` 的 `ValidateVars`），小写键会被
400 `value_invalid` 拒绝。所以安装 values 里那套 camelCase 名字在这里必须换成大写。

`PATCH /v1/nodes/{name}` 是 merge-patch + CAS（`resourceVersion` 必填；省略的字段 = 不动）：

```bash
SERVER=https://anotify.example.com      # 换成你的中枢
COOKIE='<控制台会话 cookie>'              # 浏览器里那份，别把明文写进脚本

# hw：普通用户，路径在它的 home 下
RV=$(curl -fsS -b "$COOKIE" "$SERVER/v1/nodes/hw" | jq -r .metadata.resourceVersion)
curl -fsS -X PATCH "$SERVER/v1/nodes/hw" -b "$COOKIE" \
  -H 'content-type: application/json' \
  -d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Node\",
       \"metadata\":{\"name\":\"hw\",\"resourceVersion\":\"$RV\"},
       \"spec\":{\"vars\":{\"REPO_DIR\":\"/home/zzjcool/code/anotify-agent\"}}}"

# host-ws：服务用户是 root，路径在 /root 下——**换一个值**，不要抄上面那条
RV=$(curl -fsS -b "$COOKIE" "$SERVER/v1/nodes/host-ws" | jq -r .metadata.resourceVersion)
curl -fsS -X PATCH "$SERVER/v1/nodes/host-ws" -b "$COOKIE" \
  -H 'content-type: application/json' \
  -d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Node\",
       \"metadata\":{\"name\":\"host-ws\",\"resourceVersion\":\"$RV\"},
       \"spec\":{\"vars\":{\"REPO_DIR\":\"/root/code/anotify-agent\"}}}"
```

两台一起改可以走 `POST /v1/nodes/config/batch`（逐台 CAS、逐台给结论，失败只影响那一台）。
改完核对有效值：`GET /v1/nodes/hw?namespace=<ns>` 的 `status.effectiveVars.REPO_DIR`
（local > node > namespace 逐键解析的结果）。

**升级注意**：老安装如果 values 里还带着 `repoDir`，现在会被 400 `values_invalid` 拒绝
（`spec.values` 收了 `additionalProperties: false`）。这是有意的——把机器路径留在 values 里
正是这次要去掉的东西；补上节点变量、从 values 里删掉 `repoDir` 再装。

## 验证输出（原样）

```
$ go test ./internal/playbook/... -count=1
ok  	github.com/zzjcool/anotify/internal/playbook	0.265s

$ go test ./internal/artifact/... -count=1
--- FAIL: TestAgentTUIWorktreeNewRemove (1.22s)
--- FAIL: TestAgentTUIWorktreeOff (0.40s)
--- FAIL: TestAgentTUIBranchEmptyDetaches (0.67s)
FAIL
FAIL	github.com/zzjcool/anotify/internal/artifact	11.126s

# 上面 3 个失败与本次改动无关（macOS /var → /private/var 符号链接）：
# 用 git stash 去掉本次全部改动后，在 HEAD 上跑同样 3 个测试，**同样失败**。
$ go test ./internal/artifact/... -count=1 -skip 'TestAgentTUIWorktreeNewRemove|TestAgentTUIWorktreeOff|TestAgentTUIBranchEmptyDetaches'
ok  	github.com/zzjcool/anotify/internal/artifact	8.652s

$ make lint-go                → exit 0（vet + staticcheck SA*,U1000）
$ make check-annotations      → exit 0
$ make check-test-discipline  → exit 0（五项全部与基线一致）
$ make check-comment-density  → exit 0
   扫描 753 个文件 · 注释行 42893 / 基线 42893
   目录：80 个有基线，0 个与基线不一致
   ✅ 与基线完全一致（超与降都算不一致）
$ make check-doc-links / check-yaml-doc / check-agents → 均 exit 0
```

注释棘轮**没有上调**（42893 = 42893，逐目录 0 不一致）：守卫的解释写在断言消息与 README 里，
而不是新加注释行——`internal/playbook` 与 `examples/playbooks/issue-fix/capabilities` 的
注释预算当时都是满的（后者 5 行预算全是 `#!/bin/sh`）。

**没有跑 e2e**：本机 docker 不可用，不声称跑过。

## 偏差与未决

1. **变量名是 `REPO_DIR` 而不是任务卡建议的 `repoDir`**：平台 `ValidateVars` 的键规则是
   `^[A-Z][A-Z0-9_]{0,63}$`，`vars.repoDir` 会被写入期 400 拒绝、过不了
   `artifact` 的 `unknown_field_path`。这是平台约束不是取舍，README/报告都按 `REPO_DIR` 写实。
2. **`internal/artifact` 的 3 个 TUI 测试**在本机（macOS）红，HEAD 上同样红，与本次改动无关；
   未修（不在本任务范围，属 `internal/artifact` 与环境路径规范化的事）。
3. **`make ci` 未整体跑通**：卡在 `check-wire-fields` —— 本工作区没有 `node_modules`
   （`Cannot find package 'yaml'`），脚本在 import 阶段就失败，与本次改动无关。任务卡点名的
   四道门禁已单独跑全绿。
