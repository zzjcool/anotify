# S1-D7 · 显式部署意图与回执的事务授权补口

## 只读事实与裁决

fresh G2 review + runtime-deploy-seam-facts已确认：`Hub.Deploy` 的显式nodeIDs未经namespace许可过滤；`CreateDeployments`只在事务中校验Capability/digest/Node存在，不校验Node namespace；线上可写未授权pending并发送namespace/能力引用，随后ack才拒绝。`handleDeployed`的当前credential/ns/intent refs检查不在一事务，最终`MarkDeployResult`裸写。

本补口不依赖API层可能的预检查，store权限检查必须与intent/receipt同事务。仍由S1当前唯一store writer完成；如已冻结不能恢复写，报告D7 pending由父后续序列化。禁止另起并行writer/让G2runtime前置Get代authority。

## 精确授权

1. 允许修改`internal/store/deployment.go`及直接store/v4test部署测试。
2. 当前`CreateDeployments`签名保持，**同一事务**内对全部显式Node逐一验证当前Node配置+声明允许目标namespace（复用现namespaceAllowedOn或等价真实tx helper），Capability属于ns且version/digest真实deployable；任何目标未授权/不存在全包0写，不新pending、不删除/覆盖先前合法intent。offline不是未授权，仍允许已配置授权的离线Node pending；只由explicit Deploy产生，无install/reconciler自动部署。
3. 新增：

```go
func (*DB) MarkDeployResultAuthorized(ctx context.Context, auth NodeAuthorization, in DeploymentResultInputs) error
```

inputs包含真实intent ID或等价(requestId/expecteddeploymentID)、namespace、capability versionUID/name/version/digest、合法state/error与可信serverNow。事务核当前凭证hash、namespace许可、Node绑定、当前intent未被supersede、requestId与digest完整一致，才状态落库。requestId只关联不选身份；旧凭证/撤销ns/未知intent/错request/digest/跨Node/旧receipt不能改当前intent。重复精确同结果可幂等，changedbody拒冲突（现表能够表达的字段内），不借投递帧做自动重建。
4. 不改变执行grant/renew/result/emit/cancel/升级协议，旧内部MarkDeployResult不成为新的wire入口；G2后续只能调用Authorized新入口，不能precheck+裸Mark。
5. schema如果已有intent row足以CAS，不额外迁移表；如exactbinding/error历史不足，列出具体gap父裁决，不引入独立Revision/资源reconciler。

## 新回归与原测试

- 显式未授权Node+在线/离线都0intentwrite；混合合法/非法全包0写，先前合法intent保留；合法offline生成真实pending。
- receipt旧token/撤销namespace/wrongNode/unknownrequest/wrongdigest/supersededID全部0写；合法applied/error事实往返、精确repeat幂等。
- 现G1 v4 deployment/namespace/capability局部身份断言不删不弱。若老fixture没有Node namespace权限，补真实namespace授权配置，仅改变setup并写具名原因，不改期待0写/FK/digest行为。
- S1五条固定门禁不减，全部store/race与跨runtime消费者；`CreateDeployments`新增permission guard若当前runtime/tests暴露不符合配置的调用须明确上报，不靠默认全部allow。

本补口仍store-only，同一WT一个writer；Node R9另一WT，protocol/runtime/Go依赖/FE/用户数据只读。G2之后消费，不做push/MR/merge/rebase/reset/squash或清旧证据。worker/scout不指定model/preset。
