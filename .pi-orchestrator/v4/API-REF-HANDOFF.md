# 五 spec 本地引用解析 · API2（父技术约定）

现状：T1真实五spec尚未交付，本文件示例是**具名测试fixture**，不是已实现API或要求T1刻意采用外部pathItem。T2不等待实际snippet即可验证标准OpenAPI `$ref`。T1五文件内合法相对引用必须可读，T2不得因引用形式把合法operation永久跳过。

## 范围

- 允许当前文件内部 `#/...` 和五文件集合内相对引用（如 `./resource.yaml#/components/schemas/Source`、`events.yaml#/components/pathItems/...`）；文件名仅已冻结 resource/events/agent/ingress/auth.yaml。
- `spec-dir` 根是解析边界；禁止联网、任意绝对路径、父目录逃逸或第六份未声明schema真源。错误引用明确warning/error，不能fallback旧openapi或返回空成功。
- 标准JSON Pointer支持 `~0`、`~1`，路径项与文档节点定位不得靠字符串截取猜测。引用pathItem可以位于components.pathItems（OpenAPI3.1）或其它合法可定位位置；不强迫真实spec使用pathItems扩展。
- pathItem引用链/循环或缺文件/缺指针必须有界并给具体origin/ref错误。Schema自递归与合法互相引用不是坏operation，文档无需全图无限展开；可保留引用结构或有界展示，但不能因此丢整条HTTP路径。
- 至少覆盖 pathItem、parameters、requestBody、responses 及其schema引用，做到method/path/说明/参数和请求响应文档不会因有效本五文件ref消失。实际sitegen若只展示部分schema，可保留参照但必须区分支持范围与真实缺失。
- 归属按(method,path)。同path跨events/ingress不同verb合并、不覆盖；duplicate同method/path报明确错误。operationId全局唯一。

## 具名 fixture 示例（不是API交付文件）

```yaml
# fixture api/events.yaml
openapi: 3.1.0
info: {title: Events fixture, version: v4}
paths:
  /v1/namespaces/{namespace}/events:
    $ref: '#/components/pathItems/EventRead'
components:
  pathItems:
    EventRead:
      get:
        operationId: fixtureListEvents
        parameters:
          - $ref: './resource.yaml#/components/parameters/Namespace'
        responses:
          '200':
            description: Event list
            content:
              application/json:
                schema:
                  $ref: '#/components/schemas/EventList'
  schemas:
    EventList:
      type: object
      properties:
        kind: {const: EventList}
        items: {type: array, items: {type: object}}
```

```yaml
# fixture api/ingress.yaml: 同path的POST不得覆盖上面的GET
openapi: 3.1.0
info: {title: Ingress fixture, version: v4}
paths:
  /v1/namespaces/{namespace}/events:
    post:
      operationId: fixturePostEvent
      parameters:
        - $ref: './resource.yaml#/components/parameters/Namespace'
      requestBody:
        content:
          application/json:
            schema:
              $ref: './resource.yaml#/components/schemas/SourceEventInput'
      responses:
        '200': {description: Accepted}
```

`resource.yaml` fixture明确提供components.parameters.Namespace与schemas.SourceEventInput；另外一例把pathItem置于另一个五文件并引用，用于实际跨文件pathItem测试。agent/auth fixture可以没有paths但有合法components，不伪造真实鉴权。所有fixture放T2自己的sitegen/testdata，禁止写T1 api目录。

## 实施顺序与验收

当前R1唯一Webwriter先安全收口；下一T2单writer仅sitegen/cmdsitegen相关Go与fixture做API2支持，不能与R1并发写，不能替T1生产yaml。最少测试：同path两verbs、有效本地/跨文件ref、JSON Pointer转义、坏指针/坏文件/越界/远程拒绝、pathItemcycle有界、合法recursiveSchema不丢operation、partialmissingwarn/allmissingempty。

实际T1五spec落地后父交具体HEAD给T2最终parity，不以fixture绿声称真实所有operation已对齐。不得为兼容旧接口增加备用文件名、远程加载或双shapeadapter。
