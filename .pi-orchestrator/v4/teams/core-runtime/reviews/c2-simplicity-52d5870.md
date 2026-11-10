# STRICT C2 裁决：FAIL

**固定提交：** `52d58703beac70eecb4dc338f84969cf275c87b1`
**范围：** `internal/store/{resource_cas.go,resource_version.go,capability.go,source.go,config.go,node.go,namespace.go,playbook.go}`，及支撑下述判断的 v4 测试。仅使用 `git show`。

确认 **1 个 Major，0 个 Critical**。不是“侦察完成即通过”。

## 1. 阻塞事实：名字级投影的所有权不一致

### M1 · Major — 新建历史低版本会覆盖 live latest 的 edges

**证据与调用链：**

- `CreateCapability → createCapabilityTx`：显式版本未存在时直接进入插入，没有要求该版本成为 latest。
  `internal/store/capability.go:180-195`
- `insertCapabilityOn` 插入成功后，**无条件**以新行的 `Spec.Next` 重建能力名字级出边。
  `internal/store/capability.go:263-266`
- 重建先删除该名字的全部出边，再插入传入的 next。
  `internal/store/capability.go:340-356`
- 相反，CAS 更新路径明确只有 `latest.ID == cur.ID` 才能重建这个投影。
  `internal/store/resource_cas.go:93-101`

**静态确认的反例：**

先创建 `worker@3(next=["keep"])`，再通过普通 `CreateCapability` 创建尚不存在的 `worker@1(next=["low"])`。第二次插入会把名字级 edges 换成 `["low"]`，但没有修改 `worker@3` 的正文或 resourceVersion。历史版本创建因此静默改变现行名字级投影，形成两套互相矛盾的所有权规则。

已有测试明确规定：低版本新增时，裸名解析和边投影仍属于最高版本；但它覆盖的是 **InstallPlaybook**，不是普通创建入口。
`internal/store/v4test/playbook_test.go:1324-1325,1349-1368`；安装调用另一个插入入口：`internal/store/playbook.go:373-378`。

**影响：** store 的名字级 edges 不再可靠地对应 live latest 的 `Spec.Next`。这里确认的是投影一致性缺陷，未扩大声称已经证明下游执行错误。

**最小修复要求：** 历史低版本的创建不得覆盖 latest 所有的名字级投影；创建与 CAS 更新必须遵守同一个、同事务内的 latest 所有权规则。无需重构 UID 或引入 C3 机制。

## 2. 其余冻结接口事实

- **单一 opaque token helper：** `ResourceVersion(uid, revision)`；解析拒绝裸计数器、非规范数字及额外段。冲突对象只携带完整 token，不携带 Source/config/secret。
  `internal/store/resource_version.go:10-58`；负样本：`internal/store/v4test/resource_cas_test.go:529-540`。
- **名字路径同事务 CAS：** Source 按 `(namespaceID,name)` 在事务内定位，再比较 UID+revision；参数为 `SourcePatch + expectedVersion string`，没有额外请求 UID 或 If-Match 参数。
  `internal/store/resource_cas.go:153-196`。其他指定名字入口同样调用统一 token 校验。
- **live/private pinned 边界存在：** 普通能力名字及 ID 查询过滤 `deleted_at`；不过滤的 pinned helper 未导出，并注明调用方须已有真实引用。Task 路径使用 pinned UID，不回退到 live name。
  `internal/store/capability.go:475-502,535-541`；`internal/store/node.go:1128-1135`。
- **删除仍是内部事实：** 能力删除写内部 `deleted_at`、保留正文，清除自身出边；没有新增公共 deletionTimestamp/finalizer/reconciler。
  `internal/store/resource_cas.go:111-145`。
- **Source metadata 独立：** `DisplayName` 是独立字段，PATCH 用指针区分省略与清空；省略 config 保留原字节，展示修改不重算 cron 调度事实。
  `internal/store/source.go:61-66,479-514`；测试：`internal/store/v4test/resource_cas_test.go:352-376,655-674`。
- **Node root/config 共用身份令牌来源：** root 查询读取 `node_configs.resource_version`，两个写入口共用 `putNodeConfigOn`。
  `internal/store/node.go:212-215`；`internal/store/resource_cas.go:261-345`；竞态断言：`internal/store/v4test/resource_cas_test.go:431-440`。

## 3. 裁剪后的 Minor / 待验证

- **Minor：** 旧的 `nodes.resource_version` 仍被写入，Node 字段注释仍称配置另有计数器，但 root 实际读取配置计数器；这是冗余状态及误导性注释，未确认第二个有效 CAS authority。
  `internal/store/node.go:69-70,215,593-594`。
- **序列化风险，非已确认 HTTP 泄漏：** Node/NodeConfig 的直接 JSON 序列化仍输出数值 resourceVersion；Source 空 displayName 也会输出，不使用 omitempty。上层 DTO 未纳入检查，不能把这些标签直接断言为线上泄漏。
  `internal/store/node.go:70`；`internal/store/config.go:121`；`internal/store/source.go:62`。
- C3 physical GC、FK、authorized execution、manifest 均保持 **pending**；pinned helper 的存在不等于授权执行已验证。旧上层 API 编译问题按冻结要求处理，不要求 legacy fallback。

**交付记录：** 修改文件无；新增测试无。固定提交及所读文件的 `git show` 均成功；未运行测试、构建、lint/typecheck，无执行验证结果。

{"ok": false, "reason": "STRICT C2 failed: 1 confirmed Major; ordinary lower-version CreateCapability overwrites the live latest name-level edge projection"}
