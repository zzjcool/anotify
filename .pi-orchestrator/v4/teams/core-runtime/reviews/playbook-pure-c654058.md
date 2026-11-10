# Fresh review · internal/playbook @ c654058（只读）

范围：`git show c654058:...`；未跑 build/test、未写文件；G1-5b store 事务、OCI、G3 不计 missing。

## 事实清单
- `plan.go:150-165` 12 格落点；`plan.go:175-222` 顺序 render→读快照→矩阵，冲突返回 `(result,error)`。
- `plan.go:308-339` 行 2/5 reuse：`effNext=snap.Next`，不修改 `next`；差异只进 `unapplied/note`。`plan.go:390-417` 行 9/12 对 `targets` 同口径。
- `plan.go:612-639` 仅 webhook 省略 `token` 继承已有值；`secret` 不继承。`plan.go:646-688` 差异只给字段名，不给值；`Conflict` 见 `plan.go:27-34`。
- `values.go:131-159` 默认值只补 `properties`；`render.go:320-357` 恰好单引用保原类型；`render.go:400-435` 点号路径段规则；测试见 `render_test.go:74-137,262-295,680-721`。
- `values.go:58-106` writeOnly 按 schema 递归打码；`render.go:119,169` 名字校验；`parse.go:31-59,162-199` 唯一入口 `playbook.yaml`，不要求入口再复制 values/capabilities/sources；`render.go:236-238` 渲染 files 不含入口。
- `plan.go:3-11`、`render.go:3-14`、`parse.go:3-13` 无 store/DB import；Render、Plan 均为纯函数，Plan 只依赖 `Existing`。
- `plan.go:63-77` Existing 六个只读查询，均无 `error` 返回；`helpers_test.go:274-355` 是内存 fake；c654058 内 grep 无生产 `playbook.Existing` 实现。
- 12 格逐行测试齐全：`plan_test.go:38-358`；token/secret：`plan_test.go:362-419`；回执形状：`receipt_test.go:11-74`。测试数量本身不作为正确性证据。

## 三角度结论
- **正确性**：12 格主路径、reuse 不改 next/targets、token 省略保留 vs secret 真实差异、显式差异脱敏、默认值/类型/路径、writeOnly 打码、唯一根与 render 0DB 均与父 CONTRACT/D3、W3 口径一致；未发现已证实的阻断 bug。
- **测试覆盖**：主路径覆盖好；缺口集中在「同一 foldKey 多版本」「显式低版本」「复用指令的 store 门闩」「组合冲突优先级」「writeOnly 整数组/对象」「json.Number」。
- **简洁性**：PlanResult 复用项仍携带声明侧 `Spec.Next/Targets`，store adapter 必须以 `Action` 为唯一写入门闩，否则会误写入口边。

## Findings（路径:行 / severity / 证据）
1. **MAJOR(risk)** `plan.go:63-66,262-264,318-339` — `CapabilityByFoldKey` 只返回“一行”且未规定排序；显式版本可造成同一 foldKey 对应多版本，选中的 `snap.Version/Next` 会决定 skipped 与 effNext，行 2/7 可能依赖 adapter 的任意 tie-break。无对应测试。
2. **MAJOR(risk)** `plan.go:232-240,203-206,476-492` — 行 4 显式 v 不存在时即使 `v < max` 也 INSERT；`effNext[name]=a.Spec.Next` 假设新行就是有效路由，但裸名解析取 max，可能误报/漏报 unreachable。行 4 测试用空库（`plan_test.go:104-119`）。
3. **MINOR(risk)** `plan.go:311-317,392-396` — reuse 指令仍带声明侧 `Next/Targets`；`plan_test.go:250-276,327-357` 只断言 Action。生产 adapter 若按 Spec 写边就违反“只加不改”。
4. **MINOR(risk)** `plan.go:59-77` — Existing 查询无 error 通道；DB 失败若被 adapter 映射成 `false`，看起来等同“库里没有”。G1-5b 未在本提交，无法验证。
5. **MINOR(risk)** `playbook.go:201-215` — `Package` JSON 同时暴露 `values/capabilityPaths/sourcePaths` 与 `files`；W3 要求 files 是唯一根清单，G3 DTO 不应把它序列化成第二份 `spec.values/capabilities/sources`。
6. **MINOR(coverage)** `values.go:70-73` — writeOnly 落在整个数组/对象时直接替换成 `"***"`；`render_test.go:536-580` 只测标量与数组元素内标量，无整数组/对象用例。契约字面支持“字段替换”，当前实现安全但未钉死。
7. **MINOR(coverage)** `plan.go:515-527` — 能力冲突与源冲突并存时代码一律选 `playbook_conflict`；无组合用例，契约未明确优先级。
8. **MINOR(implementation)** `plan.go:705-723` vs `sourcespec/config.go:30-43` — `jsonEqual` 用 float64，`SameConfig` 用 UseNumber；`1` vs `1.0` 的差异会退化成 `["config"]` 而非字段名，不泄露值但诊断变粗。

## 未证实（明确写风险）
- `json.Number` values：`render.go:475` 有分支，但无测试证明 YAML marshal + artifact.Parse 仍保数字类型；API 解码是否 UseNumber 未验证。
- `parse.go:203-229,254-290,366-390`：入口 kind 省略时，`playbook.yaml` 可被列入 capabilities/sources 并通过 Parse，但 Render 才失败；无测试。
- `render.go:16-21,119,169`：源名复用能力名正则 `capNamePattern`；source.md 未找到独立源名正则，若源侧规则不同则存在分叉风险。

{"ok": true, "reason": "read-only fresh review complete at c654058; 2 major unproven route-model risks, no confirmed blocking bug"}
