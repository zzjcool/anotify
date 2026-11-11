#!/usr/bin/env python3
"""五份 OpenAPI 的**信封字段级**机械断言（#333 / G3-D5）。

由 `make contract` 间接调用：`scripts/check_contract.py` 在末尾加载本文件并跑
`main()`（Makefile 配方属 G4 owner，本 issue 不改它）。单独跑也合法：

    python3 scripts/check_envelope.py

## 查什么（每条都对应一个真实漂移方式）

  E1  资源信封齐五键：`apiVersion`/`kind`/`metadata`/`spec`/`status` 必须在 `required`
      **且**在 `properties` 里；列表信封必须是 `apiVersion`/`kind`/`metadata`/`items`。
      少一个键 = 前端要写 `if (x.status)` 这类防御分支，而 v4 的契约是「status 恒存在」。
      信封的识别条件是「有 apiVersion + metadata」，`kind` 枚举必须**恰好一个值**——
      否则一个双值枚举就能把整条规则静默关闭。
  E2  camelCase：**任何层级**的 `properties` 键名（含内联嵌套对象）必须是 camelCase。
      例外只有**冻结的 snake_case 实现形状**（背压 503 的 4 个字段），且必须在这里具名
      登记——例外面可见，才不会被当成新的自由。
  E3  数组纪律：`type: array` 必须有 `items`，且数组不许 `nullable: true`
      （空集合恒 `[]` 而不是 `null` 的 schema 侧对应约束）。
  E4  `ObjectMeta` 是封闭集合：属性只能是 api-conventions §3.2 那七个，且
      `additionalProperties: false`（防 `labels`/`annotations`/`generation` 被加回来）。
  E5  `Status` 形状：`kind` 恒 `Status`、`status` 恒 `Failure`、`code` 是整数，
      且 `apiVersion`/`kind`/`status`/`code`/`reason` 都在 `required` 里。
  E6  错误响应的**族→schema 绑定**：`Status<code>` 必须指向 `Status`、`AuthError<code>`
      指向 `AuthError`、`NodeUploadError<code>` 指向 `NodeUploadError`、
      `Backpressure<code>` 指向 `BackpressureError`。抄错组件是最容易发生的
      "看起来对、其实换了形状"。
  E7  cursor 与已退役键：`ListMeta.continue` 必须是字符串；`nextCursor` / `before` /
      `sinceId` / `remainingItemCount` 不得出现在**任何 schema 属性名或参数名**里
      （v4 只有 `continue`）。
  E8  CAS：所有 `resourceVersion`（含 `$ref` 到整数 schema 的）必须是字符串；
      CAS 资源的 DELETE（能力/源/包/命名空间/节点）必须声明必填 query 参数
      `resourceVersion`——DELETE 不带请求体，令牌只能走 query。路径项写在
      `components.pathItems` 里也要看得到。

行为侧（`[]` 而非 `null` 的真实序列化、错误 envelope 的真实 HTTP 形状、路由与 DTO 的
双向对账）由 `internal/server/openapi_parity_test.go` 用真 mux/真 handler 验证——
schema 说自己对不算数。

自测（`self_test`）在每次运行时都跑：对内存 fixture 逐条注入错误，断言本检查器抓得住。
"""

from __future__ import annotations

import copy
import importlib.util
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("❌ 需要 PyYAML：pip install pyyaml", file=sys.stderr)
    sys.exit(1)

REPO_ROOT = Path(__file__).resolve().parent.parent
API_DIR = REPO_ROOT / "api"


def _load_openapi_module():
    """复用 `check_openapi.py` 的文件清单与 `$ref` 解析（不写第二份）。

    两个脚本都在 `scripts/`（不是包），所以按路径加载。共享一份解析器很重要：
    两份实现迟早会在 `./x.yaml` 的处理上分叉（review 已指出这种风险）。
    """
    path = Path(__file__).resolve().parent / "check_openapi.py"
    spec = importlib.util.spec_from_file_location("check_openapi", path)
    if spec is None or spec.loader is None:  # pragma: no cover
        raise RuntimeError(f"无法加载 {path}")
    module = importlib.util.module_from_spec(spec)
    previous = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        spec.loader.exec_module(module)
    finally:
        sys.dont_write_bytecode = previous
    return module


_OPENAPI = _load_openapi_module()
# `/v1/...` 五份文件名（短名 → 文件名）与 `$ref` 解析都只有一份实现。
SPEC_FILES: tuple[str, ...] = tuple(_OPENAPI.SPEC_FILES[name] for name in _OPENAPI.SPEC_NAMES)
split_ref = _OPENAPI.split_ref
resolve = _OPENAPI.resolve_ref

# E1：信封键。`context` 是 Node 的 `?namespace=` 顶层附加键（api-conventions §7.4 明文
# 要求它**不在** metadata/spec/status 里）；`warnings` 是可选顶层数组（§3.1）。
ENVELOPE_KEYS = {"apiVersion", "kind", "metadata", "spec", "status"}
LIST_KEYS = {"apiVersion", "kind", "metadata", "items"}
EXTRA_TOP_LEVEL_KEYS = {"context", "warnings"}

# E4：ObjectMeta 的封闭集合（api-conventions §3.2）。没有 labels/annotations/
# generation/finalizers/deletionTimestamp —— 这是刻意的，不是遗漏。
OBJECT_META_KEYS = {
    "name",
    "namespace",
    "uid",
    "version",
    "resourceVersion",
    "creationTimestamp",
    "displayName",
}

# E2：允许的 snake_case 属性名（实现冻结，具名登记）。
SNAKE_CASE_EXCEPTIONS = {
    # 背压 503 的 body 由 backpressure.go 冻结成 snake_case：字段名就是给
    # CLI / 节点侧重试器读的机器契约（`retry_after_sec`），改成 camelCase 会
    # 让已发布的调用方读不到。
    "pending_age_sec",
    "max_pending_age_sec",
    "retry_after_sec",
}

# E6：错误响应族 → 目标 schema 名。
ERROR_FAMILY_TARGET = {
    "Status": "Status",
    "AuthError": "AuthError",
    # 身份流程的 409：`{"status"}` 或 `{"error"}` 两形状（oneOf）。
    "AuthFlow": "AuthFlow",
    "NodeUploadError": "NodeUploadError",
    "Backpressure": "BackpressureError",
}

# E7：v4 已退役/未采纳的旧键（出现即说明有人按旧契约写 schema）。
RETIRED_KEYS = ("nextCursor", "before", "sinceId", "remainingItemCount")

# E8：哪些路径是「带 CAS 的资源」（DELETE 必须带 ?resourceVersion=）。
# 用正则而不是列举 5 条路径：新增 CAS 资源时正则自动覆盖，不会漏。
CAS_DELETE_PATHS = (
    re.compile(r"^/v1/namespaces/\{[^}]+\}/(capabilities|sources|playbooks)/"),
    re.compile(r"^/v1/namespaces/\{[^}]+\}$"),
    re.compile(r"^/v1/nodes/\{[^}]+\}$"),
)

CAMEL_RE = re.compile(r"^[a-z][a-zA-Z0-9]*$")
METHODS = ("get", "put", "post", "delete", "patch")


def load_docs(api_dir: Path) -> dict[str, dict]:
    docs: dict[str, dict] = {}
    for name in SPEC_FILES:
        docs[name] = yaml.safe_load((api_dir / name).read_text(encoding="utf-8"))
    return docs


def deref(node: object, docs: dict[str, dict], current: str, depth: int = 0) -> tuple[object, str]:
    """把 `$ref` / 单元素 `allOf` 外壳剥掉，拿到真实 schema。"""
    while isinstance(node, dict) and depth < 8:
        depth += 1
        if "$ref" in node:
            node, current = resolve(str(node["$ref"]), docs, current)
            continue
        all_of = node.get("allOf")
        if isinstance(all_of, list) and len(all_of) == 1:
            node, current = deref(all_of[0], docs, current, depth)
            continue
        break
    return node, current


def iter_nodes(node: object, where: str, trail: tuple[str, ...] = ()):
    """深度遍历文档，产出 (位置标签, 键路径, 节点)。"""
    yield where, trail, node
    if isinstance(node, dict):
        for key, value in node.items():
            yield from iter_nodes(value, f"{where}.{key}", trail + (str(key),))
    elif isinstance(node, list):
        for idx, value in enumerate(node):
            yield from iter_nodes(value, f"{where}[{idx}]", trail + (str(idx),))


def write_schema_names(docs: dict[str, dict]) -> set[str]:
    """收集所有被 `requestBody` 直接引用的 schema 名。

    写请求的形状**不是**响应信封：它不该有 `status`（提交前必须剥离），也不要求五键
    齐全（`metadata.resourceVersion` 在创建时就不存在）。用「被 requestBody 引用」
    来判身份，比用命名约定（`*Write`）可靠——名字可以骗人，引用关系不会。
    """
    names: set[str] = set()
    for name in SPEC_FILES:
        doc = docs.get(name) or {}
        for _, _, node in iter_nodes(doc, name):
            if not isinstance(node, dict):
                continue
            body = node.get("requestBody")
            if not isinstance(body, dict):
                continue
            for media in (body.get("content") or {}).values():
                ref = ((media or {}).get("schema") or {}).get("$ref")
                if isinstance(ref, str) and "/components/schemas/" in ref:
                    names.add(ref.rsplit("/", 1)[-1])
    return names


def path_items(doc: dict, docs: dict[str, dict], current: str) -> dict[str, dict]:
    """返回 {path: 已解析的 path item}（`components.pathItems` 的 `$ref` 也解开）。

    为什么必须解 `$ref`：agent/events/ingress 已经把 path item 放进
    `components.pathItems`，只遍历 `paths` 会把它们的 operation 全部漏掉。
    """
    items: dict[str, dict] = {}
    for path, item in (doc.get("paths") or {}).items():
        resolved, _ = deref(item, docs, current)
        if isinstance(resolved, dict):
            items[path] = resolved
    return items


def check_documents(docs: dict[str, dict]) -> list[str]:
    """纯函数：给定五份文档返回全部错误（自测靠它在内存里注入变异）。"""
    errors: list[str] = []
    writes = write_schema_names(docs)

    for name in SPEC_FILES:
        doc = docs.get(name)
        if not isinstance(doc, dict):
            errors.append(f"{name}: 缺失或不是映射")
            continue
        schemas = ((doc.get("components") or {}).get("schemas") or {})

        # ---------- E1 信封齐五键 ----------
        for schema_name, schema in schemas.items():
            if not isinstance(schema, dict) or schema_name in writes:
                continue
            props = schema.get("properties")
            if not isinstance(props, dict):
                continue
            # 信封的识别条件：有 apiVersion + metadata。**不**用 kind 枚举长度，
            # 否则把 kind 写成双值就能让以下所有规则静默关闭。
            if "apiVersion" not in props or "metadata" not in props:
                continue
            where = f"{name}:components.schemas.{schema_name}"
            kind_prop = props.get("kind")
            kind_enum = (kind_prop or {}).get("enum") if isinstance(kind_prop, dict) else None
            if kind_enum is None:
                # 没有 kind 枚举 = 不是「资源文档」（例如 dry-run 的草稿 DTO，
                # 它自带 apiVersion/metadata 但不是可寻址资源）。这里跳过，
                # 但**有**枚举时必须是单值：双值枚举等于把信封规则静默关掉。
                continue
            if not isinstance(kind_enum, list) or len(kind_enum) != 1:
                errors.append(
                    f"{where}: kind 必须恰好一个枚举值（实际 {kind_enum!r}）—— "
                    "多值会让信封规则失去判据"
                )
                continue
            kind = kind_enum[0]
            required = set(schema.get("required") or [])
            keys = LIST_KEYS if isinstance(kind, str) and kind.endswith("List") else ENVELOPE_KEYS
            missing = keys - required
            if missing:
                errors.append(f"{where}: {'列表' if keys is LIST_KEYS else '资源'}信封缺少 required 键 {sorted(missing)}")
            undeclared = (required & keys) - set(props)
            if undeclared:
                errors.append(
                    f"{where}: required 里声明了 {sorted(undeclared)}，但 properties 里没有它们"
                    "（required 悬空 = schema 自相矛盾）"
                )
            if keys is ENVELOPE_KEYS:
                unexpected = set(props) - ENVELOPE_KEYS - EXTRA_TOP_LEVEL_KEYS
                if unexpected:
                    errors.append(
                        f"{where}: 信封顶层出现未声明的键 {sorted(unexpected)}"
                        f"（只允许 {sorted(EXTRA_TOP_LEVEL_KEYS)} 这些明确例外）"
                    )
            api_version = props.get("apiVersion")
            if isinstance(api_version, dict) and api_version.get("enum") not in (None, ["anotify/v1"]):
                errors.append(
                    f"{where}: apiVersion 必须是恒量 anotify/v1，实际 {api_version.get('enum')!r}"
                )

        # ---------- E4 ObjectMeta 封闭集合 ----------
        object_meta = schemas.get("ObjectMeta")
        if isinstance(object_meta, dict):
            where = f"{name}:components.schemas.ObjectMeta"
            props = object_meta.get("properties") or {}
            unexpected = set(props) - OBJECT_META_KEYS
            if unexpected:
                errors.append(
                    f"{where}: 出现 §3.2 之外的属性 {sorted(unexpected)}"
                    "（labels/annotations/generation/finalizers 在 v4 不存在）"
                )
            if object_meta.get("additionalProperties") is not False:
                errors.append(f"{where}: 必须 additionalProperties: false")
            version = props.get("version")
            if isinstance(version, dict) and version.get("type") not in (None, "integer"):
                errors.append(f"{where}: metadata.version 必须是 integer（它不是 generation）")

        # ---------- E5 Status 形状 ----------
        status_schema = schemas.get("Status")
        if isinstance(status_schema, dict):
            where = f"{name}:components.schemas.Status"
            props = status_schema.get("properties") or {}
            required = set(status_schema.get("required") or [])
            missing = {"apiVersion", "kind", "status", "code", "reason"} - required
            if missing:
                errors.append(f"{where}: Status 缺少 required 键 {sorted(missing)}")
            if (props.get("kind") or {}).get("enum") != ["Status"]:
                errors.append(f"{where}: Status.kind 必须枚举为 [Status]")
            if (props.get("status") or {}).get("enum") != ["Failure"]:
                errors.append(
                    f"{where}: Status.status 必须枚举为 [Failure]"
                    "（成功不用 Status 表达——成功自有 2xx + 资源文档）"
                )
            if (props.get("code") or {}).get("type") != "integer":
                errors.append(f"{where}: Status.code 必须是 integer（与 HTTP 状态码同值）")

        # ---------- E7 cursor 类型 ----------
        list_meta = schemas.get("ListMeta")
        if isinstance(list_meta, dict):
            cont = (list_meta.get("properties") or {}).get("continue")
            if not isinstance(cont, dict) or cont.get("type") != "string":
                errors.append(f"{name}:components.schemas.ListMeta: continue 必须是 string")

        # ---------- E6 错误响应族→schema 绑定 ----------
        for resp_name, resp in ((doc.get("components") or {}).get("responses") or {}).items():
            family = next((f for f in ERROR_FAMILY_TARGET if resp_name.startswith(f)), None)
            if family is None:
                continue
            where = f"{name}:components.responses.{resp_name}"
            media = ((resp or {}).get("content") or {}).get("application/json") or {}
            schema_ref = (media.get("schema") or {}).get("$ref")
            target = ERROR_FAMILY_TARGET[family]
            if not isinstance(schema_ref, str):
                errors.append(f"{where}: 错误响应必须引用 {target} schema")
                continue
            resolved, _ = deref({"$ref": schema_ref}, docs, name)
            if not isinstance(resolved, dict):
                errors.append(f"{where}: $ref {schema_ref} 无法解析")
                continue
            if schema_ref.rsplit("/", 1)[-1] != target:
                errors.append(
                    f"{where}: 引用了 {schema_ref.rsplit('/', 1)[-1]!r}，按族名应为 {target!r}"
                )

        # ---------- E2 camelCase / E3 数组 / E7 退役键 / E8 RV 类型（全层级）----------
        for where, _, node in iter_nodes(doc, name):
            if not isinstance(node, dict):
                continue
            props = node.get("properties")
            if isinstance(props, dict):
                for prop_name in props:
                    if prop_name.startswith("x-") or prop_name in SNAKE_CASE_EXCEPTIONS:
                        continue
                    if not CAMEL_RE.match(prop_name):
                        errors.append(
                            f"{where}.properties.{prop_name}: 属性名不是 camelCase"
                            "（api-conventions E7；snake_case 只能在具名例外面里）"
                        )
                for retired in RETIRED_KEYS:
                    if retired in props:
                        errors.append(
                            f"{where}.properties.{retired}: 出现已退役的键"
                            "（v4 只有 metadata.continue 一种分页形状）"
                        )
                if "resourceVersion" in props:
                    rv, _ = deref(props["resourceVersion"], docs, name)
                    if isinstance(rv, dict) and rv.get("type") not in (None, "string"):
                        errors.append(
                            f"{where}.properties.resourceVersion: 必须是字符串"
                            "（不透明令牌，前端不得 parseInt）"
                        )
            if node.get("type") == "array":
                if "items" not in node:
                    errors.append(
                        f"{where}: type: array 但没有 items"
                        "（无 items 的数组在生成客户端里是 any[]，也说明 schema 没想清元素形状）"
                    )
                if node.get("nullable") is True:
                    errors.append(f"{where}: 数组不许 nullable —— 空集合必须是 []，不是 null（不变量 I9）")
            # E7 也要看参数名：`?before=` / `?sinceId=` 是同一类旧契约残留。
            params = node.get("parameters")
            if isinstance(params, list):
                for entry in params:
                    resolved, _ = deref(entry, docs, name)
                    if not isinstance(resolved, dict):
                        continue
                    pname = resolved.get("name")
                    if pname in RETIRED_KEYS:
                        errors.append(
                            f"{where}: 参数名 {pname!r} 是已退役的旧契约"
                            "（v4 只有 `continue`）"
                        )

        # ---------- E8 CAS 资源的 DELETE 必须带 query 令牌 ----------
        for path, item in path_items(doc, docs, name).items():
            if not any(p.match(path) for p in CAS_DELETE_PATHS):
                continue
            delete = item.get("delete")
            if not isinstance(delete, dict):
                continue
            params = []
            for entry in [*(item.get("parameters") or []), *(delete.get("parameters") or [])]:
                resolved, _ = deref(entry, docs, name)
                if isinstance(resolved, dict):
                    params.append(resolved)
            queries = {p.get("name") for p in params if p.get("in") == "query" and p.get("required") is True}
            if "resourceVersion" not in queries:
                errors.append(
                    f"{name}:{path} DELETE: 必须声明必填 query 参数 resourceVersion"
                    "（DELETE 不带请求体，CAS 令牌只能走 query）"
                )

    # ---------- 一致性：错误族知识只有一份 ----------
    for family in ERROR_FAMILY_TARGET:
        if not any(prefix.startswith(family) for prefix in _OPENAPI.ERROR_FAMILIES):
            errors.append(
                f"自检失败：错误族 {family!r} 在 check_openapi.ERROR_FAMILIES 里没有对应前缀"
                "（两份族知识分叉 = 一族永远不被校验）"
            )
    return errors


# --------------------------------------------------------------------------------------
# 自测
# --------------------------------------------------------------------------------------

def minimal_docs() -> dict[str, dict]:
    def resource_doc() -> dict:
        return {
            "openapi": "3.1.0",
            "info": {"title": "r", "version": "1"},
            "servers": [{"url": "/"}],
            "tags": [{"name": "t"}],
            "paths": {
                "/v1/namespaces/{name}/sources/{name}": {
                    "parameters": [],
                    "delete": {
                        "operationId": "deleteThing",
                        "tags": ["t"],
                        "security": [],
                        "parameters": [
                            {"name": "thing", "in": "path", "required": True, "schema": {"type": "string"}},
                            {"name": "namespace", "in": "path", "required": True, "schema": {"type": "string"}},
                            {
                                "name": "resourceVersion",
                                "in": "query",
                                "required": True,
                                "schema": {"type": "string"},
                            },
                        ],
                        "responses": {"204": {"description": "gone"}},
                    },
                },
                "/v1/namespaces/{name}/sources": {
                    "parameters": [],
                    "get": {
                        "operationId": "listThings",
                        "tags": ["t"],
                        "security": [],
                        "parameters": [{"name": "namespace", "in": "path", "required": True, "schema": {"type": "string"}}],
                        "responses": {"200": {"description": "ok"}},
                    },
                    "post": {
                        "operationId": "createThing",
                        "tags": ["t"],
                        "security": [],
                        "parameters": [
                            {"name": "namespace", "in": "path", "required": True, "schema": {"type": "string"}}
                        ],
                        "requestBody": {
                            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ThingWrite"}}}
                        },
                        "responses": {"201": {"description": "ok"}},
                    },
                },
            },
            "components": {
                "schemas": {
                    "ObjectMeta": {
                        "type": "object",
                        "required": ["name"],
                        "additionalProperties": False,
                        "properties": {
                            "name": {"type": "string"},
                            "resourceVersion": {"type": "string"},
                            "version": {"type": "integer"},
                        },
                    },
                    "ListMeta": {"type": "object", "properties": {"continue": {"type": "string"}}},
                    "Status": {
                        "type": "object",
                        "required": ["apiVersion", "kind", "status", "code", "reason"],
                        "properties": {
                            "apiVersion": {"type": "string", "enum": ["anotify/v1"]},
                            "kind": {"type": "string", "enum": ["Status"]},
                            "status": {"type": "string", "enum": ["Failure"]},
                            "code": {"type": "integer"},
                            "reason": {"type": "string"},
                        },
                    },
                    "AuthError": {
                        "type": "object",
                        "required": ["error"],
                        "properties": {"error": {"type": "string"}},
                    },
                    "BackpressureError": {
                        "type": "object",
                        "required": ["error"],
                        "properties": {
                            "error": {"type": "string"},
                            "retry_after_sec": {"type": "integer"},
                        },
                    },
                    "Thing": {
                        "type": "object",
                        "required": ["apiVersion", "kind", "metadata", "spec", "status"],
                        "properties": {
                            "apiVersion": {"type": "string", "enum": ["anotify/v1"]},
                            "kind": {"type": "string", "enum": ["Thing"]},
                            "metadata": {"$ref": "#/components/schemas/ObjectMeta"},
                            "spec": {"type": "object", "additionalProperties": True},
                            "status": {"type": "object", "additionalProperties": True},
                        },
                    },
                    "ThingWrite": {
                        "type": "object",
                        "required": ["apiVersion", "kind", "metadata", "spec"],
                        "properties": {
                            "apiVersion": {"type": "string", "enum": ["anotify/v1"]},
                            "kind": {"type": "string", "enum": ["Thing"]},
                            "metadata": {"type": "object", "additionalProperties": True},
                            "spec": {"type": "object", "additionalProperties": True},
                        },
                    },
                    "ThingList": {
                        "type": "object",
                        "required": ["apiVersion", "kind", "metadata", "items"],
                        "properties": {
                            "apiVersion": {"type": "string", "enum": ["anotify/v1"]},
                            "kind": {"type": "string", "enum": ["ThingList"]},
                            "metadata": {"$ref": "#/components/schemas/ListMeta"},
                            "items": {"type": "array", "items": {"$ref": "#/components/schemas/Thing"}},
                        },
                    },
                },
                "responses": {
                    "Status400": {
                        "description": "bad",
                        "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Status"}}},
                    },
                    "AuthError401": {
                        "description": "unauth",
                        "content": {"application/json": {"schema": {"$ref": "#/components/schemas/AuthError"}}},
                    },
                },
            },
        }

    docs = {"resource.yaml": resource_doc()}
    for name in SPEC_FILES:
        if name in docs:
            continue
        docs[name] = {
            "openapi": "3.1.0",
            "info": {"title": name, "version": "1"},
            "servers": [{"url": "/"}],
            "tags": [{"name": "t"}],
            "paths": {},
            "components": {
                "schemas": {"ListMeta": {"type": "object", "properties": {"continue": {"type": "string"}}}},
                "responses": {},
            },
        }
    return docs


def self_test() -> list[str]:
    """返回自测失败项（空 = 检查器本身可信）。"""
    failures: list[str] = []
    base = minimal_docs()
    baseline = check_documents(base)
    if baseline:
        failures.append(f"自测基线应为 0 错误，实际 {len(baseline)} 条：{baseline[:3]}")
        return failures

    def expect(label: str, mutate, token: str) -> None:
        docs = copy.deepcopy(base)
        mutate(docs)
        errors = check_documents(docs)
        if not any(token in err for err in errors):
            failures.append(f"自测 {label}: 没抓到（期望 {token!r}，实际 {errors[:2]}）")

    schemas = lambda docs: docs["resource.yaml"]["components"]["schemas"]  # noqa: E731

    def drop_status_prop(docs):
        schemas(docs)["Thing"]["properties"].pop("status")

    def drop_status_required(docs):
        schemas(docs)["Thing"]["required"].remove("status")

    def dual_kind_enum(docs):
        schemas(docs)["Thing"]["properties"]["kind"]["enum"] = ["Thing", "ThingAlias"]
        schemas(docs)["Thing"]["properties"].pop("status")
        schemas(docs)["Thing"]["required"].remove("status")

    def list_missing_items(docs):
        schemas(docs)["ThingList"]["properties"].pop("items")

    def nested_snake_case(docs):
        schemas(docs)["Thing"]["properties"]["spec"] = {
            "type": "object",
            "properties": {"retry_policy": {"type": "string"}},
        }

    def array_without_items(docs):
        schemas(docs)["ThingList"]["properties"]["items"] = {"type": "array"}

    def nullable_array(docs):
        schemas(docs)["ThingList"]["properties"]["items"]["nullable"] = True

    def object_meta_leaks(docs):
        schemas(docs)["ObjectMeta"]["properties"]["annotations"] = {"type": "object"}

    def status_bad_status_enum(docs):
        schemas(docs)["Status"]["properties"]["status"]["enum"] = ["Success"]

    def error_family_mismatch(docs):
        docs["resource.yaml"]["components"]["responses"]["AuthError401"]["content"]["application/json"][
            "schema"
        ]["$ref"] = "#/components/schemas/Status"

    def retired_property(docs):
        schemas(docs)["ListMeta"]["properties"]["nextCursor"] = {"type": "string"}

    def retired_param(docs):
        docs["resource.yaml"]["paths"]["/v1/namespaces/{name}/sources"]["get"]["parameters"].append(
            {"name": "before", "in": "query", "schema": {"type": "string"}}
        )

    def continue_not_string(docs):
        schemas(docs)["ListMeta"]["properties"]["continue"] = {"type": "integer"}

    def int_resource_version(docs):
        schemas(docs)["Thing"]["properties"]["metadata"] = {
            "$ref": "#/components/schemas/ObjectMetaInt"
        }
        schemas(docs)["ObjectMetaInt"] = {
            "type": "object",
            "properties": {"resourceVersion": {"type": "integer"}},
        }

    def delete_without_cas(docs):
        params = docs["resource.yaml"]["paths"]["/v1/namespaces/{name}/sources/{name}"]["delete"][
            "parameters"
        ]
        docs["resource.yaml"]["paths"]["/v1/namespaces/{name}/sources/{name}"]["delete"]["parameters"] = [
            p for p in params if p["name"] != "resourceVersion"
        ]

    def delete_in_path_items(docs):
        path = "/v1/nodes/{name}"
        docs["resource.yaml"]["paths"][path] = {"$ref": "#/components/pathItems/Hidden"}
        docs["resource.yaml"]["components"]["pathItems"] = {
            "Hidden": {
                "delete": {
                    "operationId": "hiddenDelete",
                    "tags": ["t"],
                    "security": [],
                    "parameters": [
                        {"name": "name", "in": "path", "required": True, "schema": {"type": "string"}},
                    ],
                    "responses": {"204": {"description": "gone"}},
                }
            }
        }

    expect("信封 properties 缺 status", drop_status_prop, "properties 里没有")
    expect("信封 required 缺 status", drop_status_required, "缺少 required 键")
    expect("kind 双值枚举（不得静默关闭规则）", dual_kind_enum, "恰好一个枚举值")
    expect("列表 properties 缺 items", list_missing_items, "properties 里没有")
    expect("嵌套属性 snake_case", nested_snake_case, "不是 camelCase")
    expect("数组缺 items", array_without_items, "没有 items")
    expect("数组 nullable", nullable_array, "不许 nullable")
    expect("ObjectMeta 泄漏 annotations", object_meta_leaks, "§3.2 之外")
    expect("Status.status 枚举错", status_bad_status_enum, "必须枚举为 [Failure]")
    expect("错误响应族指错 schema", error_family_mismatch, "按族名应为")
    expect("退休 cursor 属性", retired_property, "已退役的键")
    expect("退休 cursor 参数名", retired_param, "已退役的旧契约")
    expect("continue 非字符串", continue_not_string, "continue 必须是 string")
    expect("resourceVersion 经 $ref 变成整数", int_resource_version, "必须是字符串")
    expect("DELETE 缺 CAS query", delete_without_cas, "必填 query 参数 resourceVersion")
    expect("DELETE 藏在 pathItems 里", delete_in_path_items, "必填 query 参数 resourceVersion")
    return failures


def main() -> int:
    if not API_DIR.is_dir():
        print(f"❌ 找不到 {API_DIR}", file=sys.stderr)
        return 1
    try:
        docs = load_docs(API_DIR)
    except Exception as exc:  # noqa: BLE001 - 读取/解析失败都要具名报出
        print(f"❌ 读取五份 spec 失败：{exc}", file=sys.stderr)
        return 1

    errors = check_documents(docs)
    for failure in self_test():
        errors.append(f"自测失败：{failure}（检查器本身坏了，绿了也不可信）")

    if errors:
        print("❌ 信封字段级门禁失败：", file=sys.stderr)
        for err in errors:
            print("  - " + err, file=sys.stderr)
        return 1

    n_schemas = sum(
        len(((docs[name].get("components") or {}).get("schemas") or {})) for name in SPEC_FILES
    )
    print(
        f"  信封/status/cursor/CAS 字段级检查通过（{len(SPEC_FILES)} 份文件 / {n_schemas} 个 schema / "
        f"{len(SNAKE_CASE_EXCEPTIONS)} 个具名 snake_case 例外）"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
