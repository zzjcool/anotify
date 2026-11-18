# 目标
用且仅用五份真实 OpenAPI 描述 v4 已实现 HTTP/Node 端点，并把 operation、引用、鉴权、DTO/envelope 的机械对账纳入门禁。

## 依赖
{{ISSUE_V4_01}}、{{ISSUE_V4_02}} 的真实路由/DTO 冻结后完成 parity；schema 草稿可提前只读准备，不能拿假 fixture 代真接口。

## 实施范围 / 所有权
- `api/{resource,events,agent,ingress,auth}.yaml`；退役删除旧 `api/openapi.yaml`，禁止第六权威来源/旧文件 fallback。
- `scripts/check_contract.py`、`scripts/check_openapi.py`、新 `scripts/check_envelope.py`。
- `internal/server/openapi_parity_test.go` 及专属 five-spec tests；`docs/contracts/CONTRACT.md` 仅同步已冻结规则。
- Makefile、sitegen/前端、seed、runtime/store/Node 只读；使用既有 checker/Go parity 入口，Makefile 后由工具链 issue 更新。

## 必须实现
1. 按 API-SPEC-HANDOFF 归属 operation，键为 `(method, canonical path)`，operationId 唯一；同 path 不同 method 不互相覆盖。
2. 五文件内/cross-file 相对 `$ref` 可解析，支持 `components.pathItems`；禁 remote/绝对路径/逃逸/第六真源。
3. 每真实 handler 有且只有一份 spec、每 spec operation 有真实 handler；anonymous/session/ConsoleBearer/NodeBearer 鉴权与代码一致。
4. DTO/schema 双向字段核对：lightweight Resource、status readonly、camelCase、空数组、cursor、opaque CAS、writeOnly；E1 未解析失败 Task/emit `taskId` 与未知 version/digest presence 例外准确。
5. 升级旧 check_contract 的 v4 内容寻址/中枢禁 exec/上传授权边界，不通过禁用 guard 求绿。

## 验收标准
- [ ] 5 文件真实存在、仅 5 真源，旧单文件消费者不得暗中 fallback。
- [ ] 正反例覆盖重复 method/path、重复 operationId、丢路由/多路由、坏 ref/逃逸、错 security、DTO 漏字段/额外字段、错误 envelope/status/cursor。
- [ ] `python3 -m py_compile scripts/check_contract.py scripts/check_openapi.py scripts/check_envelope.py` 通过。
- [ ] 三 checker 真实运行通过，`go test ./internal/server -run TestOpenAPIParity -count=1` 通过。
- [ ] `make contract` 使用当前已有入口真实通过；如 G4 旧引用阻塞，精确记录，不修改 G4 文件越权。
- [ ] 向前端 issue 提供真实五文件与 method/path/security inventory；不重复实现已交付的五文件 sitegen reader。

## 完成定义
exact-five、全引用/operation/DTO/security/envelope 机械验收与 fresh review 通过；交真实版本与 logs。旧文件删除和前端实际输入集成必须按依赖顺序协调，不能只交五份空 YAML。