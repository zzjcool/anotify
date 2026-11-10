#!/usr/bin/env python3
"""校验 api/ 下的 **exact-five** OpenAPI 真源：归属、引用、鉴权方案、错误信封形状。

由 `make contract` 调用（v4 / #333）。这是 v3 单文件版 `check_openapi.py` 的重写：
v3 版本硬编码 `api/openapi.yaml`、只查 path 集合与 `security` 存在性，且**不解析 `$ref`**
——把 path item 写成 `$ref` 后它会被无声跳过（非方法键静默 continue），于是「拆五文件」
这件事一旦发生，门禁会变成假绿。

v4 的真源是且只是这五份文件（`docs/design/v4/api-conventions.md` §2 /
`.pi-orchestrator/v4/API-SPEC-HANDOFF.md`）：

    api/resource.yaml   api/events.yaml   api/agent.yaml   api/ingress.yaml   api/auth.yaml

本脚本查的是**静态**结构约束（不需要编译、毫秒级）：

  1. exact-five：`api/` 目录下恰好这五个 `.yaml`，且**不得**存在旧单文件
     `api/openapi.yaml`（旧文件是 v3 真源，任何消费者不得回退到它）。
  2. 归属：operation 的归属键是 **(METHOD, canonical path)**。同 path 不同 method
     不互相覆盖（`GET …/events` 在 events.yaml、`POST …/events` 在 ingress.yaml 是合法
     的两条 operation）；**同 (METHOD, canonical path) 跨文件/同文件重复**是错误。
     `operationId` 全局唯一。
  3. 路径形状：path 写真实全路径（`/v1/...`），`servers` 不再补 `/v1`；namespace 位置的
     占位符必须是 `{namespace}`（不是 `{ns}`），否则前端按名字生成的参数会分叉。
  4. 引用：五文件内部的 `#/components/...` 与**跨文件相对** `./<five>.yaml#/components/...`
     必须可解析；支持 `components.pathItems`（path item 用 `$ref` 指向 components）。
     禁止 remote（http/https/`//`）、绝对路径（`/...`）、目录逃逸（`..`）、
     以及指向第六个文件的 ref（第六真源）。
  5. 鉴权：每个 operation 必须显式声明 `security`（`security: []` = 明确匿名）；
     引用的 scheme 必须在**同一份文件**里定义；四套 scheme 的定义跨文件必须逐字一致。
  6. 错误信封：每个 4xx/5xx 响应必须 `$ref` 一个**具名**的错误响应组件，名字必须与状态码
     对齐（`Status<code>` / `AuthError<code>` / `NodeUploadError<code>`），不允许「缺 schema
     蒙过去」——例外面必须显式列名，这与 api-conventions A23 的纪律一致。

**不查**的东西（分开是为了不重复实现同一套判据）：
  - 真实路由/handler/DTO 双向对账、security 与中间件的实测一致性：
    `internal/server/openapi_parity_test.go`（`TestOpenAPIParity*`，要真 mux + reflect）。
  - 信封字段级规则（status 只读、camelCase、`[]`、cursor、CAS、writeOnly）：
    `scripts/check_envelope.py`（由 check_contract.py 串接）。
  - schema.sql 的不变量（A2/A4/A5/A6/A7）：`scripts/check_contract.py`。

自测（`self_test`）在每次运行时都跑：它对内存里的最小 fixture 逐条注入**故意写错**的
文档，断言每类错误都被抓住。检查器坏了要立刻红，而不是假绿——这条纪律沿用
`check_contract.py` 的自检注释与 `scripts/check-tmp-leaks.mjs --self-test`。
"""

from __future__ import annotations

import copy
import json
import re
import sys
from collections.abc import Iterable
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - 环境缺库时给出可执行指引
    print("❌ 需要 PyYAML：pip install pyyaml", file=sys.stderr)
    sys.exit(1)

REPO_ROOT = Path(__file__).resolve().parent.parent
API_DIR = REPO_ROOT / "api"

# exact-five：这五份，别的一律不存在（旧单文件 `openapi.yaml` 是退役真源）。
SPEC_NAMES = ("resource", "events", "agent", "ingress", "auth")
SPEC_FILES = {name: f"{name}.yaml" for name in SPEC_NAMES}
LEGACY_SPEC = "openapi.yaml"

# 路径的合法方法键（其余键在 path item 里必须落在 PATH_ITEM_KEYS 内）。
METHODS = ("get", "put", "post", "delete", "options", "head", "patch", "trace")
PATH_ITEM_KEYS = set(METHODS) | {"parameters", "summary", "description", "servers", "$ref"}

# 允许出现在 `paths` 里的非 `/v1` 路径：`/agent-login.sh` 是 `curl … | sh` 的登入口
# （cli_auth_handlers.go），api-conventions §8.5 明确把它算在 auth 域。其余非 `/v1`
# 端点（`/health`、`/debug/pprof/*`、静态兜底 `/`）**不是**产品 API，见 NON_SPEC_ROUTES。
NON_V1_API_PATHS = {"/agent-login.sh"}

# 明确不在五份 spec 范围内的真实路由，附理由（显式清单，不靠缺键蒙过）。
NON_SPEC_ROUTES = {
    "/": "静态资源 / SPA 兜底（resolveStatic），不是 API",
    "/health": "探活端点（mux.go 内联），只给 e2e/运维用，不属于产品 API 契约",
    "/debug/pprof/": "调试端点，仅 ANOTIFY_PPROF=1 时注册",
    "/debug/pprof/cmdline": "调试端点，仅 ANOTIFY_PPROF=1 时注册",
    "/debug/pprof/profile": "调试端点，仅 ANOTIFY_PPROF=1 时注册",
    "/debug/pprof/symbol": "调试端点，仅 ANOTIFY_PPROF=1 时注册",
    "/debug/pprof/trace": "调试端点，仅 ANOTIFY_PPROF=1 时注册",
}

# 显式豁免「必须有 4xx/5xx 响应」的真实端点（每条都要给出理由）。
#
# 为什么需要这条豁免而不是放宽规则：绝大多数端点的失败形状是 Status，漏声明就是漏契约；
# 但 WebSocket 升级失败由 gorilla/websocket 直接写 `text/plain` 的 400、鉴权失败发生在
# 升级**之后**（close 1008），没有任何 Status 形状可以引用——强行 $ref 一个 Status
# 会让 spec 撒谎。
NO_ERROR_RESPONSE_OPS = {
    "auth.yaml": {
        ("GET", "/v1/vapid-public-key"): "mux.go 里的内联 handler 恒 200（未配置 VAPID 时回空串），没有失败分支",
    },
    "agent.yaml": {
        ("GET", "/v1/node/stream"): "WS 升级：成功只有 101；失败是 gorilla 的 text/plain 400"
        " 或升级后 close 1008（无 Status 形状）",
    }
}

# 鉴权方案白名单。定义必须跨文件逐字一致（同一个 gateway 不能有两种 cookie 语义）。
SECURITY_SCHEMES = {
    "session": "控制台登录会话（Cookie `anotify_session`）",
    "consoleKey": "控制台 API key（`Authorization: Bearer <key>`）",
    "nodeToken": "节点身份令牌（`Authorization: Bearer <node token>`）",
    "adminSession": "登录会话 + 超管权限（两层中间件）",
}

# 安全方案的**语义**字段（跨文件一致性只比这些，description 不比）。
SCHEME_SEMANTIC_KEYS = ("type", "in", "name", "scheme", "bearerFormat")

# 错误响应组件的命名族：`<family><code>`。族以外的名字 = 错误形状没有被显式建模。
#
# 为什么需要「族」而不是只有 Status：v4 保留域里有四套**真实**的错误体，它们是
# 实现事实（不是遗漏），混进同一个 Status 会让 spec 撒谎：
#
#   Status          —— 资源域统一错误体（api-conventions §4.1），带 apiVersion/kind/code/reason
#   AuthError       —— 会话/超管/API-key 中间件与 hook 限流：`{"error":"..."}`（auth/session.go、ratelimit.go）
#   NodeUploadError —— A1 节点产物上传的封闭 4 码：`{"error":"<code>"}`（protocol/artifact_transfer.go）
#   Backpressure  —— 派发积压 503：`{error,pending_age_sec,max_pending_age_sec,retry_after_sec}`（backpressure.go）
ERROR_FAMILIES = ("Status", "AuthError", "NodeUploadError", "Backpressure")


def load_specs(api_dir: Path) -> dict[str, dict]:
    """读五份 spec（缺失/解析失败都是硬错误，交给 main 统一报）。"""
    docs: dict[str, dict] = {}
    for name, filename in SPEC_FILES.items():
        path = api_dir / filename
        raw = path.read_text(encoding="utf-8")
        doc = yaml.safe_load(raw)
        if not isinstance(doc, dict):
            raise ValueError(f"{filename} 不是 YAML 映射（顶层类型 {type(doc).__name__}）")
        docs[name] = doc
    return docs


def canonical_path(path: str) -> str:
    """归属/比较用的规范路径：所有占位符归一成 `{}`。

    归属键是 (METHOD, canonical path)：`{ns}` 与 `{namespace}` 指同一条真实路由，
    不能让占位符改名就把一条 operation 变成两条（那正是「同 path 不同 method 不互相
    覆盖」的反向漏洞）。占位符**命名**另外由 `placeholder_errors` 单独管。
    """
    out: list[str] = []
    depth = 0
    for ch in path:
        if ch == "{":
            depth += 1
            if depth == 1:
                out.append("{}")
        elif ch == "}":
            depth = max(0, depth - 1)
        elif depth == 0:
            out.append(ch)
    return "".join(out)


# Namespace **自己**的对象/子资源路径用 `{name}`（api-conventions §7.5 的
# `/v1/namespaces/{name}` / `…/config` / `…/stats`）。`{namespace}` 的约束是给
# 「被命名空间作用域化的**其它**资源」用的（`/v1/namespaces/{namespace}/tasks` 等），
# 因为那是跨资源的一致命名，而 ns 自身的名字沿 k8s 的对象段惯例。
NAMESPACE_OWN_SUBRESOURCES = {"config", "stats"}


def placeholder_errors(path: str) -> list[str]:
    """被 ns 作用域化的资源路径里，namespace 占位符必须是 `namespace`。"""
    parts = path.split("/")
    # ["", "v1", "namespaces", "{X}", "<rest>", ...]
    if len(parts) > 4 and parts[1] == "v1" and parts[2] == "namespaces":
        seg = parts[3]
        rest = parts[4]
        if rest in NAMESPACE_OWN_SUBRESOURCES:
            return []
        if seg != "{namespace}":
            return [
                f"{path}: namespace 占位符必须是 {{namespace}}，实际是 {seg!r}"
                "（前端按参数名生成，改名字等于换一套 API）"
            ]
    return []


def split_ref(ref: str) -> tuple[str, str]:
    """把 `$ref` 拆成 (文件标签, JSON 指针)。

    文件标签：``""`` = 本文件；``"resource.yaml"`` = 相对引用的五文件之一。
    """
    if "#" not in ref:
        return ref, ""
    file_part, pointer = ref.split("#", 1)
    return file_part, pointer


def ref_errors(ref: str, where: str, is_path_item: bool = False) -> list[str]:
    """单条 `$ref` 的合法性（不解析目标，只看形状）。"""
    errors: list[str] = []
    low = ref.lower()
    if low.startswith("http://") or low.startswith("https://") or ref.startswith("//"):
        errors.append(f"{where}: 禁止 remote $ref（{ref}）—— 五份 spec 必须自包含可离线解析")
        return errors
    if ref.startswith("/"):
        errors.append(f"{where}: 禁止绝对路径 $ref（{ref}）—— 只能用本文件 #/ 或相对 ./<file>.yaml#")
        return errors
    file_part, pointer = split_ref(ref)
    if file_part:
        if ".." in file_part.split("/"):
            errors.append(f"{where}: 禁止目录逃逸 $ref（{ref}）—— 不能引用 api/ 之外的文件")
            return errors
        base = file_part[len("./"):] if file_part.startswith("./") else file_part
        allowed = {f for f in SPEC_FILES.values()}
        if base not in allowed:
            errors.append(
                f"{where}: $ref 指向第六真源 {file_part!r}（只允许 {sorted(allowed)}）"
            )
            return errors
    if not pointer.startswith("/components/"):
        errors.append(
            f"{where}: $ref 的目标必须是 components 下（{ref}）"
            f"{'；path item 请写 components.pathItems' if is_path_item else ''}"
        )
    return errors


def resolve_pointer(doc: dict, pointer: str) -> object | None:
    """按 RFC6901-ish 的 JSON 指针在文档里下钻（只支持我们的 components 形状）。"""
    if not pointer.startswith("/"):
        return None
    node: object = doc
    for token in pointer.lstrip("/").split("/"):
        token = token.replace("~1", "/").replace("~0", "~")
        if not isinstance(node, dict) or token not in node:
            return None
        node = node[token]
    return node


def resolve_ref(ref: str, docs: dict[str, dict], current: str) -> tuple[object | None, str]:
    """解析一条 `$ref`，返回 (目标, 目标所在文件标签)。解析不到 → (None, "")。"""
    file_part, pointer = split_ref(ref)
    target_file = current
    if file_part:
        base = file_part[2:] if file_part.startswith("./") else file_part
        for name, filename in SPEC_FILES.items():
            if filename == base:
                target_file = name
                break
        else:
            return None, ""
    doc = docs.get(target_file)
    if doc is None:
        return None, ""
    return resolve_pointer(doc, pointer), target_file


def walk_refs(
    node: object, where: str, out: list[tuple[str, str, bool]], trail: tuple[str, ...] = ()
) -> None:
    """收集文档里全部 `$ref`（含 path item 位置标记）。

    `is_path_item` 用**键路径**判（`paths.<path>.$ref`），不数字符串里的点：
    path 本身可能带点（`/agent-login.sh`），按点数会把它判成普通引用。
    """
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "$ref" and isinstance(value, str):
                is_path_item = len(trail) == 2 and trail[0] == "paths"
                out.append((value, where, is_path_item))
            else:
                walk_refs(value, f"{where}.{key}" if where else str(key), out, trail + (str(key),))
    elif isinstance(node, list):
        for idx, value in enumerate(node):
            walk_refs(value, f"{where}[{idx}]", out, trail + (str(idx),))


def response_component_name(ref: str) -> str:
    _, pointer = split_ref(ref)
    return pointer.rsplit("/", 1)[-1]


LEGACY_SPECS = ("openapi.yaml", "openapi.yml")
SPEC_SUFFIXES = ("*.yaml", "*.yml")


def exact_five_errors_from_names(names: Iterable[str]) -> list[str]:
    """`api/` 里的文件名清单 → exact-five 违规。

    单一实现：`check_contract.py` 的 A9 也调这里，避免两套 glob/两套 message 漂移
    （review 指出过：`.yml` 后缀能同时绕过两处）。
    """
    errors: list[str] = []
    present = set(names)
    expected = set(SPEC_FILES.values())
    for name in sorted(present - expected):
        if name in LEGACY_SPECS:
            errors.append(
                f"api/{name} 仍存在：旧单文件真源已退役（exact-five），"
                "消费者不得回退到它；请删除"
            )
        else:
            errors.append(
                f"api/{name} 是第六个 spec 文件（exact-five 只允许 {sorted(expected)}）"
            )
    for name in sorted(expected - present):
        errors.append(f"api/{name} 缺失（exact-five 要求五份真实 spec 同时存在）")
    return errors


def exact_five_errors(api_dir: Path) -> list[str]:
    """目录版：`*.yaml` 与 `*.yml` 都算（换后缀不该溜过去）。"""
    names = {p.name for suffix in SPEC_SUFFIXES for p in api_dir.glob(suffix)}
    return exact_five_errors_from_names(names)


def check_documents(docs: dict[str, dict], files: dict[str, list[str]] | None = None) -> list[str]:
    """纯函数：给定五份文档（+ 可选的 api/ 目录清单）返回全部错误。

    纯函数是为了 self_test 能在内存里注入变异而**不改真实文件**。
    """
    errors: list[str] = []

    # ---------- 1. exact-five（目录清单由调用方给，self_test 里可为空） ----------
    if files is not None:
        errors.extend(exact_five_errors_from_names(files))

    # ---------- 2. 每份文档的骨架 ----------
    for name in SPEC_NAMES:
        doc = docs.get(name)
        if not isinstance(doc, dict):
            errors.append(f"{SPEC_FILES[name]}: 缺失或不是映射")
            continue
        filename = SPEC_FILES[name]
        if not str(doc.get("openapi", "")).startswith("3."):
            errors.append(f"{filename}: 缺少合法的 openapi 版本（3.x）")
        info = doc.get("info")
        if not isinstance(info, dict) or not info.get("title") or not info.get("version"):
            errors.append(f"{filename}: info.title / info.version 必须非空")
        if not isinstance(doc.get("paths"), dict) or not doc["paths"]:
            errors.append(f"{filename}: paths 必须非空（五份 spec 都要有真实 operation）")
        if not isinstance(doc.get("components"), dict) or not doc["components"]:
            errors.append(f"{filename}: components 必须非空")
        servers = doc.get("servers")
        if not isinstance(servers, list) or not servers:
            errors.append(f"{filename}: 缺少 servers 声明")
        else:
            for idx, server in enumerate(servers):
                url = str((server or {}).get("url", ""))
                if url.rstrip("/").endswith("/v1"):
                    errors.append(
                        f"{filename}: servers[{idx}].url={url!r} 不得再带 /v1 前缀"
                        "（path 已写真实全路径，否则拼出 /v1/v1/…）"
                    )
        declared_tags = {
            str(t.get("name")) for t in (doc.get("tags") or []) if isinstance(t, dict)
        }
        if not declared_tags:
            errors.append(f"{filename}: 顶层 tags 必须声明本文件用到的标签")
        declared_schemes = ((doc.get("components") or {}).get("securitySchemes") or {})

        # ---------- 3. path / path item / operation ----------
        seen_local: dict[tuple[str, str], str] = {}
        for path, item in (doc.get("paths") or {}).items():
            where = f"{filename}:{path}"
            if not isinstance(path, str) or not path.startswith("/"):
                errors.append(f"{where}: path 必须以 / 开头")
                continue
            if not (path.startswith("/v1/") or path in NON_V1_API_PATHS):
                errors.append(
                    f"{where}: path 必须写真实全路径 /v1/… "
                    f"（唯二例外：{sorted(NON_V1_API_PATHS)}）"
                )
            for msg in placeholder_errors(path):
                errors.append(msg)
            if path in NON_SPEC_ROUTES:
                errors.append(f"{where}: 该路径属于显式排除清单（{NON_SPEC_ROUTES[path]}）")

            resolved_item = item
            if isinstance(item, dict) and "$ref" in item:
                target, target_file = resolve_ref(str(item["$ref"]), docs, name)
                if target is None:
                    errors.append(f"{where}: path item 的 $ref 无法解析（{item['$ref']}）")
                    continue
                resolved_item = target
                if not isinstance(resolved_item, dict):
                    errors.append(f"{where}: path item 的 $ref 目标不是映射（{item['$ref']}）")
                    continue
            if not isinstance(resolved_item, dict):
                errors.append(f"{where}: path item 必须是映射")
                continue
            unknown_keys = set(resolved_item) - PATH_ITEM_KEYS
            if unknown_keys:
                errors.append(
                    f"{where}: path item 出现非法键 {sorted(unknown_keys)}"
                    f"（合法键：{sorted(PATH_ITEM_KEYS)}）—— 打错的方法键会被静默丢弃"
                )
            methods = [m for m in METHODS if m in resolved_item]
            if not methods:
                errors.append(f"{where}: path item 没有任何 HTTP 方法")
            errors.extend(path_parameter_errors(path, resolved_item, where, docs, name))
            for method in methods:
                op = resolved_item[method]
                op_where = f"{where} {method.upper()}"
                if not isinstance(op, dict):
                    errors.append(f"{op_where}: operation 必须是映射")
                    continue
                key = (method.upper(), canonical_path(path))
                if key in seen_local:
                    errors.append(
                        f"{op_where}: 与 {seen_local[key]} 重复（同文件同 method+path）"
                    )
                else:
                    seen_local[key] = op_where

                op_id = op.get("operationId")
                if not isinstance(op_id, str) or not op_id:
                    errors.append(f"{op_where}: 缺少 operationId（跨五文件必须唯一）")
                tags = op.get("tags")
                if not isinstance(tags, list) or not tags:
                    errors.append(f"{op_where}: 缺少 tags")
                else:
                    for tag in tags:
                        if declared_tags and tag not in declared_tags:
                            errors.append(
                                f"{op_where}: tag {tag!r} 未在顶层 tags 声明"
                                f"（已声明：{sorted(declared_tags)}）"
                            )
                if "security" not in op:
                    errors.append(
                        f"{op_where}: 缺少 security 声明"
                        "（匿名必须显式写 `security: []`，不能靠省略键）"
                    )
                else:
                    schemes = op.get("security")
                    if not isinstance(schemes, list):
                        errors.append(f"{op_where}: security 必须是列表")
                    else:
                        for entry in schemes:
                            if not isinstance(entry, dict):
                                errors.append(f"{op_where}: security 的元素必须是映射")
                                continue
                            if not entry:
                                # `{}` 是 OpenAPI 的「允许匿名」。它和具名方案写在同一条
                                # operation 上等于「既要鉴权又不用鉴权」——生成客户端会
                                # 随机选一种，必须显式二选一。
                                errors.append(
                                    f"{op_where}: security 里同时出现匿名项 {{}} 和具名方案 "
                                    "（匿名就写 security: []）"
                                )
                            for scheme in entry:
                                if scheme not in SECURITY_SCHEMES:
                                    errors.append(
                                        f"{op_where}: 未声明的安全方案 {scheme!r}"
                                        f"（白名单：{sorted(SECURITY_SCHEMES)}）"
                                    )
                                elif scheme not in declared_schemes:
                                    # scheme 是文档作用域：不能跨文件引用，用谁就得谁定义
                                    # （pathItem 从别的文件 $ref 来时，仍然以**声明 paths
                                    # 的本文档**为准——请求最终是由本文档描述的）。
                                    errors.append(
                                        f"{op_where}: 用了 {scheme!r}，但本文件 "
                                        "components.securitySchemes 未定义它"
                                        "（scheme 不能跨文件引用）"
                                    )
                responses = op.get("responses")
                if not isinstance(responses, dict) or not responses:
                    errors.append(f"{op_where}: 缺少 responses")
                    continue
                has_success = False
                for code, response in responses.items():
                    code_str = str(code)
                    # 101 是 WebSocket 升级的**唯一**成功码（/v1/node/stream）：
                    # 把它排除就等于逼 spec 造假一个不存在的 2xx。
                    if code_str.startswith("2") or code_str == "101":
                        has_success = True
                    if not isinstance(response, dict):
                        errors.append(f"{op_where}: responses.{code_str} 必须是映射")
                        continue
                    # `$ref` 响应没有兄弟 description（描述在组件里）：只有内联响应才要求写。
                    if "$ref" not in response and not response.get("description"):
                        errors.append(f"{op_where}: responses.{code_str} 缺少 description")
                    if code_str.startswith(("4", "5")):
                        ref = response.get("$ref")
                        if not isinstance(ref, str):
                            errors.append(
                                f"{op_where}: 错误响应 {code_str} 必须 $ref 具名的错误组件"
                                f"（{ERROR_FAMILIES} 族，依据状态码命名），不能内联或省略"
                            )
                            continue
                        cname = response_component_name(ref)
                        if not any(
                            cname == f"{family}{code_str}" for family in ERROR_FAMILIES
                        ):
                            errors.append(
                                f"{op_where}: 错误响应 {code_str} 的组件名是 {cname!r}，"
                                f"应为 {'/'.join(f'{f}{code_str}' for f in ERROR_FAMILIES)}"
                                " 之一（名字与状态码对齐，否则同一状态码会有多种形状）"
                            )
                if not has_success:
                    errors.append(f"{op_where}: 至少需要一个 2xx 响应")
                has_error = any(str(code).startswith(("4", "5")) for code in responses)
                if not has_error and not any(
                    (method.upper(), canonical_path(path)) in ops for ops in NO_ERROR_RESPONSE_OPS.values()
                ):
                    errors.append(
                        f"{op_where}: 没有声明任何 4xx/5xx 响应"
                        "（失败是 API 的一部分；确实没有 Status 形状的端点请登记在 "
                        "NO_ERROR_RESPONSE_OPS 里并写明理由）"
                    )

    # ---------- 5. 归属（跨文件）与 operationId 全局唯一 ----------
    owner: dict[tuple[str, str], str] = {}
    op_ids: dict[str, str] = {}
    for name in SPEC_NAMES:
        doc = docs.get(name)
        if not isinstance(doc, dict):
            continue
        filename = SPEC_FILES[name]
        for path, item in (doc.get("paths") or {}).items():
            resolved_item = item
            if isinstance(item, dict) and "$ref" in item:
                resolved_item, _ = resolve_ref(str(item["$ref"]), docs, name)
            if not isinstance(resolved_item, dict):
                continue
            for method in METHODS:
                op = resolved_item.get(method)
                if not isinstance(op, dict):
                    continue
                key = (method.upper(), canonical_path(str(path)))
                where = f"{filename}:{path} {method.upper()}"
                if key in owner:
                    errors.append(
                        f"{where}: (method, path) 已被 {owner[key]} 占用"
                        "（同 method+path 只能出现在一份文件里；同 path 不同 method 才是合法的）"
                    )
                else:
                    owner[key] = where
                op_id = op.get("operationId")
                if isinstance(op_id, str) and op_id:
                    if op_id in op_ids:
                        errors.append(
                            f"{where}: operationId {op_id!r} 重复（另见 {op_ids[op_id]}）"
                            "—— 生成客户端会互相覆盖"
                        )
                    else:
                        op_ids[op_id] = where

    # ---------- 6. $ref 形状 + 可解析性 ----------
    for name in SPEC_NAMES:
        doc = docs.get(name)
        if not isinstance(doc, dict):
            continue
        refs: list[tuple[str, str, bool]] = []
        walk_refs(doc, "", refs)
        for ref, where, is_path_item in refs:
            loc = f"{SPEC_FILES[name]}:{where or '<root>'}"
            shape_errors = ref_errors(ref, loc, is_path_item)
            if shape_errors:
                errors.extend(shape_errors)
                continue
            target, _ = resolve_ref(ref, docs, name)
            if target is None:
                errors.append(f"{loc}: $ref 无法解析（{ref}）—— 悬空引用比没有引用更糟")

    # ---------- 7. 跨文件 scheme 定义必须一致 ----------
    definitions: dict[str, tuple[str, dict]] = {}
    for name in SPEC_NAMES:
        doc = docs.get(name) or {}
        schemes = ((doc.get("components") or {}).get("securitySchemes") or {})
        for scheme, definition in schemes.items():
            if scheme not in SECURITY_SCHEMES:
                errors.append(
                    f"{SPEC_FILES[name]}: components.securitySchemes.{scheme} 不在白名单"
                    f"（{sorted(SECURITY_SCHEMES)}）"
                )
                continue
            # 只比**语义**字段：description 是散文，五份文件各自写会漂出无意义 diff，
            # 而 type/in/name/scheme 一旦分叉就是真 bug（同一个网关两种 cookie 语义）。
            semantic = {
                k: v for k, v in (definition or {}).items() if k in SCHEME_SEMANTIC_KEYS
            }
            normalized = json.dumps(semantic, sort_keys=True, ensure_ascii=False)
            if scheme in definitions:
                first_file, first_norm = definitions[scheme]
                if first_norm != normalized:
                    errors.append(
                        f"{SPEC_FILES[name]}: securitySchemes.{scheme} 与 "
                        f"{first_file} 里的语义不一致（type/in/name/scheme 必须相同）"
                    )
            else:
                definitions[scheme] = (SPEC_FILES[name], normalized)
    for scheme in SECURITY_SCHEMES:
        if scheme not in definitions:
            errors.append(f"五份 spec 都没有定义安全方案 {scheme!r}（白名单里的方案必须至少在一处定义）")

    return errors


def declared_path_parameters(node: object, docs: dict[str, dict], current: str) -> dict[str, set[str]]:
    """收集某个 parameters 列表里声明的 path 参数（名字 → 是否 required）。

    `$ref`（含跨文件）一并解析：`parameters` 常常从共享组件来，
    “ref 了但没声明”与“压根没写”必须被同等对待。
    """
    declared: dict[str, set[str]] = {}
    if not isinstance(node, list):
        return declared
    for entry in node:
        target = entry
        if isinstance(entry, dict) and "$ref" in entry:
            target, _ = resolve_ref(str(entry["$ref"]), docs, current)
        if not isinstance(target, dict):
            continue
        if target.get("in") != "path":
            continue
        name = target.get("name")
        if isinstance(name, str) and name:
            required = "required" if target.get("required") is True else "optional"
            declared.setdefault(name, set()).add(required)
    return declared


def path_parameter_errors(
    path: str, item: dict, where: str, docs: dict[str, dict], current: str
) -> list[str]:
    """路径里的 `{param}` 必须有对应 path 参数声明，反之声明不能悬空。

    为什么查这条：OpenAPI 消费者（生成客户端 / 文档页）靠它拼 URL。缺声明的
    占位符会生成一个没有实参的函数，多声明一个 path 参数则会让调用方以为自己
    传了个不存在的段。两边都是**静默**错。
    """
    errors: list[str] = []
    placeholders = re.findall(r"\{([^}]+)\}", path)
    item_declared = declared_path_parameters(item.get("parameters"), docs, current)
    for method in METHODS:
        op = item.get(method)
        if not isinstance(op, dict):
            continue
        declared = {k: set(v) for k, v in item_declared.items()}
        for name, marks in declared_path_parameters(op.get("parameters"), docs, current).items():
            declared.setdefault(name, set()).update(marks)
        for placeholder in placeholders:
            if placeholder not in declared:
                errors.append(
                    f"{where} {method.upper()}: 路径占位符 {{{placeholder}}} 没有对应的 "
                    "path 参数声明（生成客户端会拼不出 URL）"
                )
        for name in declared:
            if name not in placeholders:
                errors.append(
                    f"{where} {method.upper()}: 声明了 path 参数 {name!r}，路径里却没有 "
                    f"{{{name}}}（悬空参数）"
                )
    return errors


# --------------------------------------------------------------------------------------
# 自测：每次运行都注入故意的错误，断言检查器抓得住
# --------------------------------------------------------------------------------------

def minimal_docs() -> dict[str, dict]:
    """最小 fixture：两份文件，跨文件 schema ref + pathItems + 四套 scheme 定义齐全。"""

    def scheme(kind: str, name: str) -> dict:
        if kind == "cookie":
            return {"type": "apiKey", "in": "cookie", "name": name}
        return {"type": "http", "scheme": "bearer"}

    schemes = {
        "session": scheme("cookie", "anotify_session"),
        "consoleKey": scheme("bearer", ""),
        "nodeToken": scheme("bearer", ""),
        "adminSession": scheme("cookie", "anotify_session"),
    }

    def op(op_id: str, security: list) -> dict:
        return {
            "operationId": op_id,
            "tags": ["t"],
            "security": security,
            "responses": {
                "200": {"description": "ok"},
                "404": {"$ref": "./resource.yaml#/components/responses/Status404"},
            },
        }

    resource = {
        "openapi": "3.1.0",
        "info": {"title": "r", "version": "1"},
        "servers": [{"url": "/"}],
        "tags": [{"name": "t"}],
        "paths": {
            "/v1/namespaces/{namespace}/things": {
                "parameters": [{"$ref": "#/components/parameters/NamespacePath"}],
                "get": op("listThings", []),
            },
            "/v1/namespaces/{namespace}/things/{name}": {
                "$ref": "#/components/pathItems/ThingItem"
            },
        },
        "components": {
            "securitySchemes": schemes,
            "schemas": {
                "Status": {"type": "object"},
            },
            "responses": {"Status404": {"description": "not found"}},
            "parameters": {
                "NamespacePath": {
                    "name": "namespace",
                    "in": "path",
                    "required": True,
                    "schema": {"type": "string"},
                },
                "ThingNamePath": {
                    "name": "name",
                    "in": "path",
                    "required": True,
                    "schema": {"type": "string"},
                },
            },
            "pathItems": {
                "ThingItem": {
                    "parameters": [
                        {"$ref": "#/components/parameters/NamespacePath"},
                        {"$ref": "#/components/parameters/ThingNamePath"},
                    ],
                    "get": op("getThing", [{"session": []}]),
                }
            },
        },
    }
    ingress = {
        "openapi": "3.1.0",
        "info": {"title": "i", "version": "1"},
        "servers": [{"url": "/"}],
        "tags": [{"name": "t"}],
        "paths": {
            "/v1/namespaces/{namespace}/things": {
                "parameters": [{"$ref": "./resource.yaml#/components/parameters/NamespacePath"}],
                "post": op("createThing", []),
            },
        },
        "components": {
            "securitySchemes": schemes,
            "schemas": {"Reuse": {"$ref": "./resource.yaml#/components/schemas/Status"}},
            "responses": {},
            "parameters": {},
            "pathItems": {},
        },
    }
    def stub(name: str) -> dict:
        """其它三份文件的占位文档：路径与 operationId 必须各自唯一（否则基线就红了）。"""
        return {
            "openapi": "3.1.0",
            "info": {"title": "x", "version": "1"},
            "servers": [{"url": "/"}],
            "tags": [{"name": "t"}],
            "paths": {f"/v1/stub-{name}": {"get": op(f"stub{name.capitalize()}", [])}},
            "components": {
                "securitySchemes": schemes,
                "schemas": {"S": {"type": "object"}},
                "responses": {},
                "pathItems": {},
            },
        }

    docs = {"resource": resource, "ingress": ingress}
    for name in SPEC_NAMES:
        docs.setdefault(name, stub(name))
    return docs


def self_test() -> list[str]:
    """返回自测失败项（空 = 检查器本身可信）。"""
    failures: list[str] = []

    base = minimal_docs()
    baseline_errors = check_documents(base)
    if baseline_errors:
        failures.append(f"自测基线应为 0 错误，实际 {len(baseline_errors)} 条：{baseline_errors[:3]}")
        return failures

    def expect(name: str, mutate, token: str, files: dict[str, list[str]] | None = None) -> None:
        docs = copy.deepcopy(base)
        mutate(docs)
        errors = check_documents(docs, files)
        if not any(token in err for err in errors):
            failures.append(f"自测 {name}: 没抓到（期望错误里出现 {token!r}，实际 {errors[:3]}）")

    def dup_method_path(docs: dict) -> None:
        docs["ingress"]["paths"]["/v1/namespaces/{namespace}/things"] = {
            "get": {
                "operationId": "listThings2",
                "tags": ["t"],
                "security": [],
                "responses": {"200": {"description": "ok"}},
            }
        }

    def dup_method_path_same_file(docs: dict) -> None:
        # 同文件内重复：靠 {ns} 改名绕不过规范路径归一
        docs["resource"]["paths"]["/v1/namespaces/{ns}/things"] = {
            "get": {
                "operationId": "listThings3",
                "tags": ["t"],
                "security": [],
                "responses": {"200": {"description": "ok"}},
            }
        }

    def dup_operation_id(docs: dict) -> None:
        docs["ingress"]["paths"]["/v1/namespaces/{namespace}/things"]["post"]["operationId"] = "listThings"

    def dangling_ref(docs: dict) -> None:
        docs["resource"]["components"]["schemas"]["Broken"] = {
            "$ref": "#/components/schemas/DoesNotExist"
        }

    def escape_ref(docs: dict) -> None:
        docs["resource"]["components"]["schemas"]["Broken"] = {
            "$ref": "../internal/store/schema.sql#/components/schemas/X"
        }

    def remote_ref(docs: dict) -> None:
        docs["resource"]["components"]["schemas"]["Broken"] = {
            "$ref": "https://example.com/spec.yaml#/components/schemas/X"
        }

    def sixth_source(docs: dict) -> None:
        docs["resource"]["components"]["schemas"]["Broken"] = {
            "$ref": "./extra.yaml#/components/schemas/X"
        }

    def missing_security(docs: dict) -> None:
        del docs["resource"]["paths"]["/v1/namespaces/{namespace}/things"]["get"]["security"]

    def bad_scheme(docs: dict) -> None:
        docs["resource"]["paths"]["/v1/namespaces/{namespace}/things"]["get"]["security"] = [
            {"token": []}
        ]
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["security"] = [
            {"token": []}
        ]

    def undeclared_scheme(docs: dict) -> None:
        # 用了 adminSession，但本文件没定义它（scheme 不能跨文件引用）
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["security"] = [
            {"adminSession": []}
        ]
        del docs["resource"]["components"]["securitySchemes"]["adminSession"]

    def bad_path_item_key(docs: dict) -> None:
        docs["resource"]["components"]["pathItems"]["ThingItem"]["gets"] = docs["resource"][
            "components"
        ]["pathItems"]["ThingItem"].pop("get")

    def bad_error_shape(docs: dict) -> None:
        docs["resource"]["paths"]["/v1/namespaces/{namespace}/things"]["get"]["responses"]["404"] = {
            "description": "inline"
        }

    def wrong_error_name(docs: dict) -> None:
        docs["resource"]["paths"]["/v1/namespaces/{namespace}/things"]["get"]["responses"]["409"] = {
            "$ref": "#/components/responses/Status404"
        }

    def v1_in_servers(docs: dict) -> None:
        docs["resource"]["servers"] = [{"url": "/v1"}]

    def missing_path_parameter(docs: dict) -> None:
        # 路径里的 {namespace} 没有声明 → 生成客户端拼不出 URL 且不报错
        docs["resource"]["components"]["pathItems"]["ThingItem"].pop("parameters")

    def orphan_path_parameter(docs: dict) -> None:
        # 声明了一个路径里不存在的 path 参数
        docs["resource"]["paths"]["/v1/namespaces/{namespace}/things"]["parameters"].append(
            {
                "name": "ghost",
                "in": "path",
                "required": True,
                "schema": {"type": "string"},
            }
        )

    def bad_namespace_placeholder(docs: dict) -> None:
        body = docs["events"]["paths"].pop("/v1/stub-events")
        docs["events"]["paths"]["/v1/namespaces/{ns}/things"] = body

    def relative_path(docs: dict) -> None:
        body = docs["events"]["paths"].pop("/v1/stub-events")
        docs["events"]["paths"]["/things"] = body

    def drop_2xx(docs: dict) -> None:
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["responses"] = {
            "404": {"$ref": "./resource.yaml#/components/responses/Status404"}
        }

    def drop_all_errors(docs: dict) -> None:
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["responses"] = {
            "200": {"description": "ok"}
        }

    def non_spec_route(docs: dict) -> None:
        docs["events"]["paths"]["/health"] = {
            "get": {
                "operationId": "healthProbe",
                "tags": ["t"],
                "security": [],
                "responses": {"200": {"description": "ok"}},
            }
        }

    def undeclared_tag(docs: dict) -> None:
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["tags"] = ["nope"]

    def mixed_anonymous_security(docs: dict) -> None:
        docs["resource"]["components"]["pathItems"]["ThingItem"]["get"]["security"] = [
            {},
            {"session": []},
        ]

    expect("重复 method+path 跨文件", dup_method_path, "已被")
    expect("重复 method+path 同文件（{ns} 改名绕不过）", dup_method_path_same_file, "重复（同文件同 method+path）")
    expect("重复 operationId", dup_operation_id, "operationId")
    expect("悬空 $ref", dangling_ref, "无法解析")
    expect("逃逸 $ref", escape_ref, "逃逸")
    expect("remote $ref", remote_ref, "remote")
    expect("第六真源 $ref", sixth_source, "第六真源")
    expect("缺 security", missing_security, "缺少 security 声明")
    expect("未知 scheme", bad_scheme, "安全方案")
    expect("scheme 未在本文件定义", undeclared_scheme, "未定义它")
    expect("path item 打错方法键", bad_path_item_key, "非法键")
    expect("错误响应缺具名组件", bad_error_shape, "必须 $ref")
    expect("错误响应组件名与状态码不符", wrong_error_name, "组件名")
    expect("servers 重复 /v1", v1_in_servers, "/v1 前缀")
    expect("namespace 占位符被改名", bad_namespace_placeholder, "占位符")
    expect("path 缺 /v1 前缀", relative_path, "真实全路径")
    expect("路径占位符缺 path 参数声明", missing_path_parameter, "没有对应的")
    expect("悬空的 path 参数声明", orphan_path_parameter, "悬空参数")
    expect("没有 2xx 响应", drop_2xx, "至少需要一个 2xx 响应")
    expect("没有 4xx/5xx 响应", drop_all_errors, "没有声明任何 4xx/5xx 响应")
    expect("spec 里出现非 spec 路由", non_spec_route, "显式排除清单")
    expect("用了未在顶层声明的 tag", undeclared_tag, "未在顶层 tags 声明")
    expect("匿名与具名方案混写", mixed_anonymous_security, "同时出现匿名项")

    # alias/rest 两个「负向目录清单」case：第六文件与旧单文件
    expect(
        "旧单文件 fallback 仍在",
        lambda docs: None,
        "旧单文件真源已退役",
        files={**{f: [] for f in SPEC_FILES.values()}, LEGACY_SPEC: []},
    )
    expect(
        "第六个 spec 文件",
        lambda docs: None,
        "第六个 spec 文件",
        files={**{f: [] for f in SPEC_FILES.values()}, "extra.yaml": []},
    )
    return failures


def main() -> int:
    if not API_DIR.is_dir():
        print(f"❌ 找不到 {API_DIR}", file=sys.stderr)
        return 1

    files = sorted({p.name for suffix in SPEC_SUFFIXES for p in API_DIR.glob(suffix)})
    try:
        docs = load_specs(API_DIR)
    except Exception as exc:  # noqa: BLE001 - 任何读取/解析失败都要具名报出
        print(f"❌ 读取五份 spec 失败：{exc}", file=sys.stderr)
        return 1

    errors = check_documents(docs, files={name: [] for name in files})

    self_test_failures = self_test()
    for failure in self_test_failures:
        errors.append(f"自测失败：{failure}（检查器本身坏了，绿了也不可信）")

    if errors:
        print("❌ OpenAPI 五文件门禁失败：", file=sys.stderr)
        for err in errors:
            print("  - " + err, file=sys.stderr)
        return 1

    n_ops = 0
    op_ids = []
    for name in SPEC_NAMES:
        doc = docs[name]
        for path, item in (doc.get("paths") or {}).items():
            resolved = item
            if isinstance(item, dict) and "$ref" in item:
                resolved, _ = resolve_ref(str(item["$ref"]), docs, name)
            if not isinstance(resolved, dict):
                continue
            for method in METHODS:
                if method in resolved:
                    n_ops += 1
                    op_ids.append(resolved[method].get("operationId"))
    print(
        f"  OpenAPI exact-five 通过（{len(SPEC_NAMES)} 份文件 / {n_ops} 个 operation / "
        f"{len(set(op_ids))} 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）"
    )
    print("  路由/handler/DTO 双向对账由 internal/server/openapi_parity_test.go 负责；"
          "信封字段规则由 scripts/check_envelope.py 负责")
    return 0


if __name__ == "__main__":
    sys.exit(main())
