// Package v4test 是 store 的 **v4 外部测试包**（package v4test）：只用 store 的导出
// API 跑 v4 契约用例——capability 的授权/slot、playbook 安装与回执、emit 与执行派发、
// 迁移与保留期形状——断言的是 v4 文档里的规则，不是内部实现细节。
//
// 为什么在包外：v4 baseline 与能力领域是一次整体重建，而 `internal/store` 内部
// 的旧测试（v3 语义：端口 / 状态机 / handler / 图版本）要到 G1-6 才统一迁移。
// 在包内新增 v4 测试会与那些夹具抢同一个 package 命名空间、也会把「测试包能不能
// 编译」变成 G1-2 的阻塞项。放到外部测试包后：
//
//   - 只允许用**导出 API**（这本身就是一条约束：v4 的领域语义必须能通过公开
//     接口表达，不能靠读私有字段证明）；
//   - `go test ./internal/store/v4test/ -count=1` 独立可跑；
//   - G1-6 做内部测试迁移闭包时不会与这里冲突。
//
// 断言纪律：不 skip、不弱化。每条用例都在验一条 v4 契约（capability.md §2/§3、§8，
// playbook.md §0，config.md 的保留期形状），没有「先跑通再说」的空壳。
//
// 本注释原先写在 helpers_test.go 顶部：测试文件里的 `// Package` 不会被 `go list`
// 与 `go doc` 收集（.Doc 只看非测试文件），所以搬到本文件，让包职责对外可见。
package v4test
