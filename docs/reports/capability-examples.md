> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

# 交付报告 · 能力样例（`run.workDir`）

**issue**：https://github.com/zzjcool/anotify/issues/472
**分支**：`pi-subagent/hw-nj/anotify-examples-f55a9d5c`

## 缺口

`run.workDir` 是「在已有仓库里跑」的入口 —— `docs/agent-install.md` 的 Docker / 原生对比表自己
就拿「能在你的项目目录里跑 `make test`」当原生安装的卖点 —— 但仓库里没有任何一份带 `workDir`
的东西可抄：

- `internal/artifact/templates.go` 的内置模板没有一个使用 `run.workDir`（`script.blank` 只有
  一行注释提到 `run.env` + `fieldRef`）；
- `examples/` 下只有 `playbooks/pr-triage-notify`，它是 playbook 包，不是单份能力；
- 想要一份「在项目目录里跑 `make test`」的 YAML，只能去翻已移除的设计文档的示例段
  —— 那份文档没有执行者复算，改坏了没人知道。

## 交付

| 文件 | 作用 |
|---|---|
| `examples/capabilities/run-in-repo.yaml` | 在已有仓库目录里跑：`requires.values` → `run.env` → `run.workDir: $(vars.SOURCE_ROOT)` |
| `examples/capabilities/run-per-param-dir.yaml` | 按参数分目录：`params.project` + `pattern` + `run.workDir: /srv/work/$(params.project)` |
| `internal/artifact/examples_test.go` | 把样例真的喂给 `artifact.Parse`，并要求每个样例都带 `workDir` |
| `scripts/check-examples.mjs` | 样例检查 + 变异自证，接进 `make ci` |
| `docs/agent-install.md`、`internal/artifact/templates.go` 注释 | 两处指路到样例（原第三处是设计文档，已随设计文档移除） |

### 样例各证明什么

1. **`run-in-repo.yaml` —— 机器事实进目录。** `requires.values: [vars.SOURCE_ROOT]` 是声明，
   `run.env.SOURCE_ROOT.valueFrom.fieldRef` 是取值，`run.workDir: $(vars.SOURCE_ROOT)` 是落点，
   脚本里 `cd "$SOURCE_ROOT"` 再把「目录从哪来」显式写一遍；`ANOTIFY_PARAM_TEST_TARGET` 演示
   可选参数（省略 = 默认值）。用户抄走它只需要做一件事：把机器 / ns 的 `SOURCE_ROOT` 配上。
2. **`run-per-param-dir.yaml` —— 参数进目录。** `workDir` 只接受 `$(params.X)`，且 X 必须先在
   `spec.params` 里声明；脚本一启动就在 `/srv/work/<project>`，不需要也不应该自己 `cd`。
   `pattern` 描述参数契约（字母数字开头、点段不能为空），把「像路径但不是目录名」的值排除在契约外。

两份样例都写明了用户抄之前必须知道的三件事：目录必须**事先存在**（节点不创建，不存在 =
`workdir_unavailable`）；不得与 GC 根（系统清理根）重叠；`run.files` 的同名文件**会被覆盖且改权限**
（0600，入口 0700）。

## 检查怎么接的

`make ci` → `make check-examples` → `node scripts/check-examples.mjs`：

1. 静态下限（防假绿）：样例数 ≥ 2；每个样例必须含 `workDir:`；每个样例的 key 数 ≥ 18
   （实测 30 / 29，删掉半份样例就会掉下来）。样例目录空了直接退出码 2，不会「绿着空跑」。
2. 执行：`go test ./internal/artifact -run TestExampleCapabilities`。仓库里已有的样例门是
   `internal/playbook/examples_test.go`（「示例包必须真过 Parse / Render / Plan」），所以这里复用
   同款思路，把样例交给 `internal/artifact` 的 `Parse`，本脚本不复制解析逻辑。
3. `--self-test` 在临时副本上做三处破坏，每一处都必须让第 2 步失败并出现对应短码；未破坏的副本
   必须通过（证明判据不是恒红）。变异本身没生效（样例改了形状导致找不到可破坏的结构）时退出码 2，
   逼人同步更新变异实现，而不是静默跳过。

## 变异结果

| 变异 | 期望 | 实际（`node scripts/check-examples.mjs --self-test`） |
|---|---|---|
| 删掉 `run.workDir` | 失败，输出含 `workDir` | ✅ 失败：`run-in-repo.yaml 没有 run.workDir` |
| 删掉 `requires.values` | 失败，输出含 `undeclared_need` | ✅ 失败：`undeclared_need: run.env 的 SOURCE_ROOT 引用了 vars.SOURCE_ROOT，但没写进 spec.requires.values` |
| 删掉被引用的 `params.project` | 失败，输出含 `undefined_param_ref` | ✅ 失败：`undefined_param_ref: run.workDir 引用了 $(params.project)，但 spec.params 里没有声明这个参数` |

## 验收记录

- `make check-examples`：exit 0（静态下限 + `artifact.Parse` + 三处变异全部符合预期）。
- `flock /tmp/anotify-ci.lock -c "make ci"`：exit 0。
- `check-annotations` 棘轮保持 `unbacked 300 / unmarked 385`（零余量，未上调基线）。

## 发现的产品问题（未在本 PR 修，按边界不动校验逻辑）

当时的**「参数 schema 建议用 `pattern` 把 `..` / `/` 挡在写入期**」这条说法（出处已随设计文档移除），
在服务端没有任何一处执行 `pattern`：中枢的派发期参数对齐（`internal/route/params.go` 的
`parseParamSchema`）只实现 `type / properties / required / default` 子集，注释里明确写着
`pattern` 等关键字「被忽略而不是被当成失败」。

实际后果与边界：

- 不会逃逸：节点展开 `workDir` 后要求「规范绝对路径 + 真实存在目录」，`../etc`、`a/b` 这类值
  会让任务以 `workdir_unavailable` 失败（`cmd/anode/internal/execenv` 的 `ExpandWorkDir`）；
- 但 `pattern` 目前只是**契约描述**（控制台 / 调用方按它校验），不是服务端 400。
  样例注释按这个事实写，没有沿用设计文档的措辞。要修的话应在写路径执行参数 schema 子集，
  与本 PR 的「只加样例与检查、不改校验逻辑」边界冲突。

## 遗留风险

- 样例里的 `SOURCE_ROOT` / `/srv/work` 是机器相关路径：抄走时必须换成目标节点真实存在的目录，
  样例注释已写明「目录必须事先存在」。
- `examples/capabilities` 目前只有两份样例；key 数下限（18）与样例数下限（2）是防退化下限，
  不是「样例要写多长」的目标。
