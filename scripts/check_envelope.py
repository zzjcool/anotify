#!/usr/bin/env python3
"""五份 OpenAPI 的**信封字段级**机械断言（#333 / G3-D5）。

由 `make contract` 间接调用：`scripts/check_contract.py` 在末尾 `import` 本文件并跑
`main()`（Makefile 配方属 G4 owner，本 issue 不改它）。单独跑也合法：

    python3 scripts/check_envelope.py

## 查什么（每条都对应一个真实漂移方式）

  E1  资源信封齐五键：`apiVersion`/`kind`/`metadata`/`spec`/`status` 必须在 `required` 里；
      列表信封必须是 `apiVersion`/`kind`/`metadata`/`items`。少一个键 = 前端要写
      `if (x.status)` 这类防御分支，而 v4 的契约是「status 恒存在」。
  E2  camelCase：每个 schema 的属性名必须是 camelCase。例外只有**冻结的 snake_case
      实现形状**（背压 503 的 4 个字段），且必须在这里具名登记——例外面可见，才不会被
      当成新的自由。
  E3  数组纪律：`type: array` 必须有 `items`，且数组不许 `nullable: true`
      （空集合恒 `[]` 而不是 `null` 的 schema 侧对应约束）。
  E4  `ObjectMeta` 是封闭集合：属性只能是 §3.2 那七个，且 `additionalProperties: false`
      （防 `labels`/`annotations`/`generation` 被悄悄加回来）。
  E5  `Status` 形状：`kind` 恒 `Status`、`status` 恒 `Failure`、`code` 是整数，
      且 `apiVersion`/`kind`/`status`/`code`/`reason` 都在 `required` 里。
  E6  错误响应的**族→schema 绑定**：`Status<code>` 必须指向 `Status`、`AuthError<code>`
      指向 `AuthError`、`NodeUploadError<code>` 指向 `NodeUploadError`、
      `Backpressure<code>` 指向 `BackpressureError`。抄错组件是最容易发生的
      "看起来对、其实换了形状"。
  E7  cursor 与已退役键：`ListMeta.continue` 必须是字符串；`nextCursor` / `before` /
      `sinceId` / `remainingItemCount` 这些 v3/wire 旧键**不得**出现在任何 schema
      属性名或参数名里（v4 只有 `continue`）。
  E8  CAS：所有 `resourceVersion` 必须是**字符串**（不是整数、不是 generation）；
      `api/resource.yaml` 里每个 DELETE 必须声明必填 query 参数 `resourceVersion`
      （DELETE 不带请求体，令牌只能走 query）。

行为侧（`[]` 而非 `null` 的真实序列化、错误 envelope 的真实 HTTP 形状）由
`internal/server/openapi_parity_test.go` 用真 handler 验证——schema 说自己对不算数。

自测（`self_test`）在每次运行时都跑：对内存 fixture 逐条注入错误，断言本检查器抓得住。
"""

from __future__ import annotations

import copy
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

SPEC_FILES = ("resource.yaml", "events.yaml", "agent.yaml", "ingress.yaml", "auth.yaml")

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
    "NodeUploadError": "NodeUploadError",
    "Backpressure": "BackpressureError",
}

# E7：v4 已退役/未采纳的旧键（出现即说明有人按旧契约写 schema）。
RETIRED_KEYS = ("nextCursor", "before", "sinceId", "remainingItemCount")

CAMEL_RE = re.compile(r"^[a-z][a-zA-Z0-9]*$")


def load_docs(api_dir: Path) -> dict[str, dict]:
    docs: dict[str, dict] = {}
    for name in SPEC_FILES:
        docs[name] = yaml.safe_load((api_dir / name).read_text(encoding="utf-8"))
    return docs


def split_ref(ref: str) -> tuple[str, str]:
    if "#" not in ref:
        return ref, ""
    return ref.split("#", 1)


def resolve(ref: str, docs: dict[str, dict], current: str) -> tuple[object | None, str]:
    file_part, pointer = split_ref(ref)
    target = current
    if file_part:
        base = file_part[2:] if file_part.startswith("./") else file_part
        if base not in SPEC_FILES:
            return None, ""
        target = base
    node: object = docs.get(target) or {}
    if not pointer.startswith("/"):
        return None, ""
    for token in pointer.lstrip("/").split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        if not isinstance(node, dict) or token not in node:
            return None, ""
        node = node[token]
    return node, target


def deref(node: object, docs: dict[str, dict], current: str, depth: int = 0) -> tuple[object, str]:
    """把 `$ref` / `allOf` 外壳剥掉，拿到真实 schema。"""
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


def walk_schemas(node: object, where: str, out: list[tuple[str, dict]]):
    """遍历文档里所有 schema 节点（带位置标签）。"""
    if isinstance(node, dict):
        yield where, node
        for key, value in node.items():
            yield from walk_schemas(value, f"{where}.{key}", out)
    elif isinstance(node, list):
        for idx, value in enumerate(node):
            yield from walk_schemas(value, f"{where}[{idx}]", out)


def write_schema_names(docs: dict[str, dict]) -> set[str]:
    """收集所有被 `requestBody` 直接引用的 schema 名。

    写请求的形状**不是**响应信封：它不该有 `status`（提交前必须剥离），也不要求五键
    齐全（`metadata.resourceVersion` 在创建时就不存在）。用「被 requestBody 引用」
    来判身份，比用命名约定（`*Write`）可靠——名字可以骗人，引用关系不会。
    """
    names: set[str] = set()

    def collect(node: object, current: str, depth: int = 0) -> None:
        if depth > 6 or not isinstance(node, dict):
            return
        body = node.get("requestBody")
        if isinstance(body, dict):
            for media in (body.get("content") or {}).values():
                schema = (media or {}).get("schema") or {}
                ref = schema.get("$ref")
                if isinstance(ref, str) and "/components/schemas/" in ref:
                    names.add(ref.rsplit("/", 1)[-1])
        for value in node.values():
            collect(value, current, depth + 1)

    for name in SPEC_FILES:
        collect(docs.get(name) or {}, name)
    return names


def check_documents(docs: dict[str, dict]) -> list[str]:
    """纯函数：给定五份文档返回全部错误（自测靠它在内存里注入变异）。"""
    errors: list[str] = []

    for name in SPEC_FILES:
        doc = docs.get(name)
        if not isinstance(doc, dict):
            errors.append(f"{name}: 缺失或不是映射")
            continue
        schemas = ((doc.get("components") or {}).get("schemas") or {})
        writes = write_schema_names(docs)

        for schema_name, schema in schemas.items():
            if not isinstance(schema, dict):
                continue
            props = schema.get("properties")
            if not isinstance(props, dict):
                continue
            where = f"{name}:components.schemas.{schema_name}"

            # ---------- E1 信封齐五键 ----------
            kind_prop = props.get("kind")
            kind_enum = (kind_prop or {}).get("enum") if isinstance(kind_prop, dict) else None
            # 信封识别：单值 kind 枚举 + 有 metadata（Status 也有 kind/apiVersion，但它是
            # 错误体，不是资源信封 —— 少了这个条件会把 Status 判成"缺 spec/status"）。
            if (
                isinstance(kind_enum, list)
                and len(kind_enum) == 1
                and "metadata" in props
                and schema_name not in writes
            ):
                kind = kind_enum[0]
                required = set(schema.get("required") or [])
                if isinstance(kind, str) and kind.endswith("List"):
                    missing = LIST_KEYS - required
                    if missing:
                        errors.append(
                            f"{where}: 列表信封缺少 required 键 {sorted(missing)}"
                            "（列表恒有 apiVersion/kind/metadata/items）"
                        )
                    undeclared = (required & LIST_KEYS) - set(props)
                    if undeclared:
                        errors.append(
                            f"{where}: required 里声明了 {sorted(undeclared)}，"
                            "但 properties 里没有它们（required 悬空 = schema 自相矛盾）"
                        )
                else:
                    missing = ENVELOPE_KEYS - required
                    if missing:
                        errors.append(
                            f"{where}: 资源信封缺少 required 键 {sorted(missing)}"
                            "（五键必须恒存在；status 无事实时给 {}）"
                        )
                    undeclared = (required & ENVELOPE_KEYS) - set(props)
                    if undeclared:
                        errors.append(
                            f"{where}: required 里声明了 {sorted(undeclared)}，"
                            "但 properties 里没有它们（required 悬空 = schema 自相矛盾）"
                        )
                    unexpected = set(props) - ENVELOPE_KEYS - EXTRA_TOP_LEVEL_KEYS
                    if unexpected:
                        errors.append(
                            f"{where}: 信封顶层出现未声明的键 {sorted(unexpected)}"
                            f"（只允许 {sorted(EXTRA_TOP_LEVEL_KEYS)} 这些明确例外）"
                        )
                api_version = props.get("apiVersion")
                if isinstance(api_version, dict) and api_version.get("enum") != ["anotify/v1"]:
                    errors.append(
                        f"{where}: apiVersion 必须是恒量 anotify/v1，实际 {api_version.get('enum')!r}"
                    )

            # ---------- E2 camelCase ----------
            for prop_name in props:
                if prop_name.startswith("x-"):
                    continue
                if prop_name in SNAKE_CASE_EXCEPTIONS:
                    continue
                if not CAMEL_RE.match(prop_name):
                    errors.append(
                        f"{where}: 属性名 {prop_name!r} 不是 camelCase"
                        "（api-conventions E7；snake_case 只能在具名例外面里）"
                    )

            # ---------- E4 ObjectMeta 封闭集合 ----------
            if schema_name == "ObjectMeta":
                unexpected = set(props) - OBJECT_META_KEYS
                if unexpected:
                    errors.append(
                        f"{where}: 出现 §3.2 之外的属性 {sorted(unexpected)}"
                        "（labels/annotations/generation/finalizers 在 v4 不存在）"
                    )
                if schema.get("additionalProperties") is not False:
                    errors.append(
                        f"{where}: 必须 additionalProperties: false —— 否则前端会以为能塞任意键"
                    )
                version = props.get("version")
                if isinstance(version, dict) and version.get("type") != "integer":
                    errors.append(
                        f"{where}: metadata.version 必须是 integer（它不是 generation，也不是字符串）"
                    )

            # ---------- E5 Status 形状 ----------
            if schema_name == "Status":
                required = set(schema.get("required") or [])
                missing = {"apiVersion", "kind", "status", "code", "reason"} - required
                if missing:
                    errors.append(f"{where}: Status 缺少 required 键 {sorted(missing)}")
                if (props.get("kind") or {}).get("enum") != ["Status"]:
                    errors.append(f"{where}: Status.kind 必须枚举为 [Status]")
                if (props.get("status") or {}).get("enum") != ["Failure"]:
                    errors.append(
                        f"{where}: Status.status 必须枚举为 [Failure]"
                        "（Success 不用 Status 表达——成功自有 2xx + 资源文档）"
                    )
                if (props.get("code") or {}).get("type") != "integer":
                    errors.append(f"{where}: Status.code 必须是 integer（与 HTTP 状态码同值）")

        # ---------- E3 数组纪律 / E7 退役键 ----------
        for where, node in walk_schemas(doc, name, []):
            if not isinstance(node, dict):
                continue
            node_type = node.get("type")
            if node_type == "array":
                if "items" not in node:
                    errors.append(
                        f"{where}: type: array 但没有 items"
                        "（无 items 的数组在生成客户端里是 any[]，也说明 schema 没想清元素形状）"
                    )
                if node.get("nullable") is True:
                    errors.append(
                        f"{where}: 数组不许 nullable —— 空集合必须是 []，不是 null（不变量 I9）"
                    )
            props = node.get("properties")
            if isinstance(props, dict):
                for retired in RETIRED_KEYS:
                    if retired in props:
                        errors.append(
                            f"{where}: 出现已退役的键 {retired!r}"
                            "（v4 只有 metadata.continue 一种分页形状）"
                        )
            # E8：resourceVersion 只能是字符串
            if isinstance(props, dict) and "resourceVersion" in props:
                rv = props["resourceVersion"]
                if isinstance(rv, dict) and rv.get("type") == "integer":
                    errors.append(
                        f"{where}: resourceVersion 必须是字符串（不透明令牌，前端不得 parseInt）"
                    )

        # ---------- E7 cursor 类型 ----------
        list_meta = schemas.get("ListMeta")
        if isinstance(list_meta, dict):
            cont = (list_meta.get("properties") or {}).get("continue")
            if not isinstance(cont, dict) or cont.get("type") != "string":
                errors.append(f"{name}:components.schemas.ListMeta: continue 必须是 string")

        # ---------- E6 错误响应族→schema 绑定 ----------
        responses = ((doc.get("components") or {}).get("responses") or {})
        for resp_name, resp in responses.items():
            family = next((f for f in ERROR_FAMILY_TARGET if resp_name.startswith(f)), None)
            if family is None:
                continue
            content = (resp or {}).get("content") or {}
            json_body = content.get("application/json") or {}
            schema_ref = (json_body.get("schema") or {}).get("$ref")
            target = ERROR_FAMILY_TARGET[family]
            if not isinstance(schema_ref, str):
                errors.append(
                    f"{name}:components.responses.{resp_name}: 错误响应必须引用 {target} schema"
                )
                continue
            resolved, _ = deref({"$ref": schema_ref}, docs, name)
            if not isinstance(resolved, dict):
                errors.append(
                    f"{name}:components.responses.{resp_name}: $ref {schema_ref} 无法解析"
                )
                continue
            # 目标名字取 ref 的最后一段（形状对了但指错组件，是真实会犯的错）
            if schema_ref.rsplit("/", 1)[-1] != target:
                errors.append(
                    f"{name}:components.responses.{resp_name}: 引用了 "
                    f"{schema_ref.rsplit('/', 1)[-1]!r}，按族名应为 {target!r}"
                )

        # ---------- E8 DELETE 必须带 query CAS ----------
        if name == "resource.yaml":
            for path, item in (doc.get("paths") or {}).items():
                if not isinstance(item, dict):
                    continue
                delete = item.get("delete")
                if not isinstance(delete, dict):
                    continue
                params = []
                for entry in list(delete.get("parameters") or []):
                    resolved, _ = deref(entry, docs, name)
                    if isinstance(resolved, dict):
                        params.append(resolved)
                queries = {
                    p.get("name")
                    for p in params
                    if p.get("in") == "query" and p.get("required") is True
                }
                if "resourceVersion" not in queries:
                    errors.append(
                        f"{name}:{path} DELETE: 必须声明必填 query 参数 resourceVersion"
                        "（DELETE 不带请求体，CAS 令牌只能走 query）"
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
                "/v1/things/{name}": {
                    "parameters": [
                        {"name": "name", "in": "path", "required": True, "schema": {"type": "string"}}
                    ],
                    "delete": {
                        "operationId": "deleteThing",
                        "tags": ["t"],
                        "security": [],
                        "parameters": [
                            {
                                "name": "resourceVersion",
                                "in": "query",
                                "required": True,
                                "schema": {"type": "string"},
                            }
                        ],
                        "responses": {"204": {"description": "gone"}},
                    },
                }
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
                    "ListMeta": {
                        "type": "object",
                        "properties": {"continue": {"type": "string"}},
                    },
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
                    "ThingList": {
                        "type": "object",
                        "required": ["apiVersion", "kind", "metadata", "items"],
                        "properties": {
                            "apiVersion": {"type": "string", "enum": ["anotify/v1"]},
                            "kind": {"type": "string", "enum": ["ThingList"]},
                            "metadata": {"$ref": "#/components/schemas/ListMeta"},
                            "items": {
                                "type": "array",
                                "items": {"$ref": "#/components/schemas/Thing"},
                            },
                        },
                    },
                },
                "responses": {
                    "Status400": {
                        "description": "bad",
                        "content": {
                            "application/json": {
                                "schema": {"$ref": "#/components/schemas/Status"}
                            }
                        },
                    },
                    "AuthError401": {
                        "description": "unauth",
                        "content": {
                            "application/json": {
                                "schema": {"$ref": "#/components/schemas/AuthError"}
                            }
                        },
                    },
                },
            },
        }

    resource = resource_doc()
    docs = {"resource.yaml": resource}
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
                "schemas": {
                    "ListMeta": {"type": "object", "properties": {"continue": {"type": "string"}}}
                },
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

    def drop_status_key(docs):
        props = docs["resource.yaml"]["components"]["schemas"]["Thing"]["properties"]
        props.pop("status")

    def list_missing_items(docs):
        props = docs["resource.yaml"]["components"]["schemas"]["ThingList"]["properties"]
        props.pop("items")

    def snake_case_prop(docs):
        docs["resource.yaml"]["components"]["schemas"]["Thing"]["properties"]["retry_after"] = {
            "type": "integer"
        }

    def array_without_items(docs):
        docs["resource.yaml"]["components"]["schemas"]["ThingList"]["properties"]["items"] = {
            "type": "array"
        }

    def nullable_array(docs):
        docs["resource.yaml"]["components"]["schemas"]["ThingList"]["properties"]["items"][
            "nullable"
        ] = True

    def object_meta_leaks(docs):
        docs["resource.yaml"]["components"]["schemas"]["ObjectMeta"]["properties"][
            "annotations"
        ] = {"type": "object"}

    def status_bad_kind(docs):
        docs["resource.yaml"]["components"]["schemas"]["Status"]["properties"]["kind"]["enum"] = [
            "Fail"
        ]

    def error_family_mismatch(docs):
        docs["resource.yaml"]["components"]["responses"]["AuthError401"]["content"][
            "application/json"
        ]["schema"]["$ref"] = "#/components/schemas/Status"

    def retired_cursor(docs):
        docs["resource.yaml"]["components"]["schemas"]["ListMeta"]["properties"][
            "nextCursor"
        ] = {"type": "string"}

    def int_resource_version(docs):
        docs["resource.yaml"]["components"]["schemas"]["ObjectMeta"]["properties"][
            "resourceVersion"
        ] = {"type": "integer"}

    def delete_without_cas(docs):
        docs["resource.yaml"]["paths"]["/v1/things/{name}"]["delete"]["parameters"] = []

    expect("信封 properties 缺 status", drop_status_key, "properties 里没有")
    expect("列表 properties 缺 items", list_missing_items, "properties 里没有")
    expect("属性名 snake_case", snake_case_prop, "不是 camelCase")
    expect("数组缺 items", array_without_items, "没有 items")
    expect("数组 nullable", nullable_array, "不许 nullable")
    expect("ObjectMeta 泄漏 annotations", object_meta_leaks, "§3.2 之外")
    expect("Status.kind 形状错", status_bad_kind, "Status.kind")
    expect("错误响应族指错 schema", error_family_mismatch, "按族名应为")
    expect("退休 cursor 键", retired_cursor, "已退役的键")
    expect("resourceVersion 写成整数", int_resource_version, "必须是字符串")
    expect("DELETE 缺 CAS query", delete_without_cas, "必填 query 参数 resourceVersion")
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
