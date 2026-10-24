#!/usr/bin/env python3
"""校验 api/openapi.yaml 与 internal/server/mux.go 的端点数一致。

由 `make contract` 调用。

为什么需要：openapi 是给外部调用方看的契约，一旦和真实路由漂移，
调用方会按文档写到不存在的端点（或者漏掉新增的端点）。
人工维护必然漂移——历史上已经漏了 9 个事件中心端点，
以及 19 个 operation 没有 security 声明。

检查项：
  1. mux 里的每条 /v1 路由，spec 里都要有对应 path（允许前缀挂载展开）
  2. spec 里的每个 path，mux 里都要能匹配到（防止文档多写）
  3. 每个 operation 都必须有 security（哪怕是 `security: []` 表示匿名）
"""

import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    print("❌ 需要 PyYAML：pip install pyyaml", file=sys.stderr)
    sys.exit(1)

MUX = Path("internal/server/mux.go")
SPEC = Path("api/openapi.yaml")

# servers: /v1 —— spec 里的 path 是相对 /v1 的
API_PREFIX = "/v1"


def mux_routes() -> set[str]:
    """从 mux.go 提取真实路由（绝对路径，含不在 /v1 下的少数端点）。"""
    src = MUX.read_text(encoding="utf-8")
    routes: set[str] = set()
    for m in re.finditer(r'mux\.Handle\("([^"]+)"', src):
        h = m.group(1)
        # 去掉方法前缀（"GET /v1/xxx"）
        path = re.sub(r"^(GET|POST|PUT|PATCH|DELETE|HEAD)\s+", "", h)
        if path == "/":
            continue
        routes.add(path)
    return routes


def spec_paths(doc: dict) -> list[str]:
    return list(doc.get("paths", {}).keys())


def spec_to_absolute(p: str) -> str:
    """把 spec 的相对路径转成 /v1 下的绝对路径。"""
    if p.startswith("/../"):
        # 明确标记的「不在 /v1 下」的端点
        return p[3:]
    if p.startswith("/"):
        return API_PREFIX + p
    return API_PREFIX + "/" + p


def matches(real: str, spec_abs: str) -> bool:
    """判断真实路由是否被某条 spec path 覆盖。

    支持两种形态：
      - 完全相等
      - spec 用 {id} 占位，真实路由用前缀挂载（如 /v1/devices/ 覆盖 /v1/devices/{id}）
    """
    if real == spec_abs:
        return True
    # spec 里有 {xxx} → 把它变成通配，看真实路由是否能匹配
    if "{" in spec_abs:
        parts = re.split(r"(\{[^}]+\})", spec_abs)
        pat = "".join("[^/]+" if x.startswith("{") else re.escape(x) for x in parts)
        return re.fullmatch(pat, real) is not None
    # 前缀挂载：mux 用 "/v1/devices/" 兜住 "/v1/devices/{id}"
    return real.rstrip("/") + "/" == spec_abs or real == spec_abs.rstrip("/") + "/"


def main() -> int:
    if not MUX.exists() or not SPEC.exists():
        print(f"❌ 找不到 {MUX} 或 {SPEC}", file=sys.stderr)
        return 1

    doc = yaml.safe_load(SPEC.read_text(encoding="utf-8"))
    real = mux_routes()
    specs = {spec_to_absolute(p): p for p in spec_paths(doc)}
    spec_abs_set = set(specs)

    errors: list[str] = []

    # 前缀挂载的根（mux 用 "/v1/admin/" 兜住所有 /v1/admin/*）
    # 注意 "/v1/admin"（无尾斜杠）是**另一条**路由，需要单独有文档。
    prefix_mounts = {r for r in real if r.endswith("/")}

    def covered_by_prefix_mount(spec_abs: str) -> bool:
        """spec 里的 path 是否落在某个前缀挂载下。"""
        return any(spec_abs.startswith(pm) for pm in prefix_mounts)

    # 1. 真实路由必须有文档
    undocumented = []
    for r in sorted(real):
        if r in spec_abs_set:
            continue
        # 前缀挂载根本身：只要它下面有任何 spec path 就算覆盖
        if r in prefix_mounts and any(s.startswith(r) for s in spec_abs_set):
            continue
        if any(matches(r, s) for s in spec_abs_set):
            continue
        undocumented.append(r)

    for r in undocumented:
        errors.append(f"mux 有、openapi 缺：{r}")

    # 2. 文档不能多写不存在的端点
    for s_abs, s_rel in sorted(specs.items()):
        if s_abs in real:
            continue
        if any(matches(r, s_abs) for r in real):
            continue
        # 落在前缀挂载下（/v1/auth/login 由 "/v1/auth/" 处理）
        if covered_by_prefix_mount(s_abs):
            continue
        # spec 用 {id}，mux 用同名前缀挂载（/v1/devices/{id} ← "/v1/devices/"）
        base = s_abs.split("{")[0].rstrip("/")
        if base + "/" in real or base in real:
            continue
        errors.append(f"openapi 多写（mux 里没有）：{s_rel}")

    # 3. 每个 operation 必须有 security（`security: []` 表示明确匿名）
    for path, ops in doc.get("paths", {}).items():
        for method, op in ops.items():
            if method not in ("get", "post", "put", "patch", "delete", "head"):
                continue
            if "security" not in op:
                errors.append(f"{method.upper()} {path} 缺少 security 声明")

    if errors:
        print("❌ openapi 与 mux 不一致：", file=sys.stderr)
        for e in errors:
            print("  - " + e, file=sys.stderr)
        return 1

    n_ops = sum(
        1
        for ops in doc.get("paths", {}).values()
        for m in ops
        if m in ("get", "post", "put", "patch", "delete", "head")
    )
    print(f"  openapi 与 mux 一致（{len(real)} 条路由 / {len(specs)} 个 path / {n_ops} 个 operation）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
