# 机器路径回归节点变量：`repoDir` 出安装 values

> 状态：进行中（本文件先落盘，作为「要改什么」的清单；完成后回填验证输出）。

## 问题

`examples/playbooks/issue-fix` 把仓库检出目录做成**安装参数** `values.repoDir`。
一份安装 values 对所有节点生效，所以多节点时必须让每台机器用**同一个绝对路径**
（靠"把目录抄成一模一样"活着）。这是把机器事实塞进了应用配置。

## 要改什么（清单）

1. `examples/playbooks/issue-fix/capabilities/{triage,implement,review,fix}.yaml`：
   `run.env.REPO_DIR` 从字面值 `{{values.repoDir}}` 改成
   `valueFrom.fieldRef.fieldPath: vars.REPO_DIR`，并在 `spec.requires.values` 声明它。
2. `examples/playbooks/issue-fix/playbook.yaml`：`spec.values` 去掉 `repoDir`
   （含 `required`），否则安装期仍会要一个机器路径。
3. `examples/playbooks/issue-fix/values.demo.json`：去掉 `repoDir`。
4. `examples/playbooks/issue-fix/README.md`：写明「每台节点 `spec.vars.REPO_DIR`
   由运维自己配」，给出 `PATCH /v1/nodes/{name}` 的设置方法与缺配时的失败语义。
5. `examples/playbooks/issue-solve/README.md`、`examples/playbooks/pr-triage-notify/README.md`：
   写一句同口径的排查结论（都不含绝对机器路径）。
6. `internal/playbook/examples_test.go`：在 `TestExamplePackagesRender` 里加守卫——
   渲染后 `run.env` 的字面值不得出现 `/home/`、`/Users/`、`/root/`；
   渲染结果再过 `artifact.Validate`，断言 0 problem（`requirements.values` 漏声明
   会被 `undeclared_need` 咬住，不另写一套判定）。

## 待回填

- [ ] 变异自证：`valueFrom` 改回字面路径 → 守卫红
- [ ] 变异自证：`requires.values` 删掉 → 守卫红
- [ ] `go test ./internal/playbook/... ./internal/artifact/... -count=1`
- [ ] `make lint-go`
- [ ] `make check-annotations check-test-discipline check-comment-density`
- [ ] 运维步骤（具体命令）
