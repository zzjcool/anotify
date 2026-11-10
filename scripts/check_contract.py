#!/usr/bin/env python3
"""CONTRACT.md §9 的静态不变量断言（v4 边界）+ 串接 `check_envelope.py`。

由 `make contract` 调用，是 `make ci` 的前置门禁。

## 为什么重写（#333）

v3 版本的 A5/A6/A7 三条断言盯的是**已经退役的东西**，于是门禁在 v4 里恒红，
而恒红的门禁等于没有门禁（没人再看它）：

| 旧断言 | v3 盯的东西 | v4 事实 |
|---|---|---|
| A5 | `CENTRAL_PACKAGES` 列表里含 `internal/executor` | 中枢执行器包已删除（v4 只有节点侧执行） |
| A6 | `capability_versions.content_hash` | 列名是 `digest`（内容寻址换了名字） |
| A7 | `cmd/agent` 不提 `node_capabilities` | 节点产品是 `cmd/anode`；`node_capabilities` 表本身已退役 |

**不靠删 guard 求绿**：每一条都换成 v4 的等价守卫，覆盖面只增不减：

  A2 (I1)  中枢不持有「怎么执行」：schema 里不得出现 argv/shell/script（保留）
  A4 (I3)  kind 不落库：能力/节点/边三张表不得有 kind 列（保留，去掉已退役表）
  A5 (I1)  中枢侧**一切包**不得 import os/exec（包清单改成动态枚举，删不掉、加不错）
  A6 (I1)  内容寻址：能力行/部署/产物两表都必须有内容标识列（换成 v4 真实列名）
  A7 (I1)  节点不得持有能力归属写路径：`cmd/anode/**` 不得出现归属表名
  A8 (A1)  上传授权/registry 边界：上传端点必须是 node token（不是会话），且
           中枢不直接写 `task_artifact_*`（registry 只经 store 的授权事务）
  A9       五份 spec 的**存在性**：`api/` 下恰好五份、旧单文件已删除（防“回退旧真源”）

A8 为什么必须机械可查：A1 的授权语义（attempt/lease/attemptToken/相位/撤销/命名空间）
全在 store 的事务里复核，一旦有人为了「先收字节」把它挪到 handler 的裸 INSERT，行为
看起来一样、迟到文件却能落库（G3-D3 的硬语义）。A8 检查「谁在写 registry」。

信封字段级规则（status 只读、camelCase、`[]`、cursor、CAS、具名错误形状）由
`scripts/check_envelope.py` 负责，本脚本在末尾串接它——`make contract` 的 Makefile
配方属 G4 owner，本 issue 不改配方，所以由这里调用，而不是新增一个 Make 目标。
"""

from __future__ import annotations

import importlib.util
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
SCHEMA = REPO_ROOT / "internal" / "store" / "schema.sql"
API_DIR = REPO_ROOT / "api"

# A4 管辖的表：能力定义、节点、路由投影。这三张是「kind 应该由声明推导而不是存储」
# 的地方（契约 §5）。`sources.kind` 是**合法**的（源类型是用户给的一等字段），
# 所以必须按表名限定，不能全库 grep。
KIND_FORBIDDEN_TABLES = ("capability_versions", "nodes", "edges")

# A2 的关键词（不变量 I1：中枢不知道"怎么执行"，只知道"做什么"）。
A2_KEYWORDS = re.compile(r"argv|shell|script", re.IGNORECASE)

# A6 管辖的表 → 它的「内容标识」列。内容寻址是「可审计」的全部来源：
#   capability_versions  能力正文的 canonical sha256（v4 的列名是 digest）
#   deployments          「某节点持有某 digest」这条事实钉住的内容身份
#   task_artifact_uploads / _manifests  产物字节的 sha256（A1）
DIGEST_COLUMN_TABLES = {
    "capability_versions": "digest",
    "deployments": "digest",
    "task_artifact_uploads": "sha256",
    "task_artifact_manifests": "sha256",
}

# A7 管辖的目录：节点侧产品（v4 是 cmd/anode）。节点不能持有「谁能拥有哪个能力」的
# 写路径——归属只由中枢的控制台部署派生。
NODE_SIDE_DIR = "cmd/anode"
ATTRIBUTION_TABLES = ("capability_versions", "deployments")

# A8：中枢侧不得直接写产物的 registry 表（只经 store 的授权事务）。
ARTIFACT_TABLES = ("task_artifact_uploads", "task_artifact_manifests", "task_artifact_objects")
CENTRAL_SERVER_DIR = "internal/server"
UPLOAD_ROUTE_MARKER = "protocol.NodeTaskArtifactsPathPrefix"

# A9：exact-five。
SPEC_FILES = ("resource.yaml", "events.yaml", "agent.yaml", "ingress.yaml", "auth.yaml")
LEGACY_SPEC = "openapi.yaml"


def parse_tables(sql: str) -> dict[str, list[tuple[int, str]]]:
    """解析出 {表名: [(行号, 该行内容)]}。

    按 `CREATE TABLE ... (` 开始、括号配对结束来切分，这样能正确处理
    `IF NOT EXISTS`、双引号表名（sqlite_master 里 `ALTER ... RENAME` 之后就是带引号的）、
    跨行列定义与注释。用 awk/grep 解析会把 `IF` 当表名——A4 曾经因此永远匹配不到。
    """
    tables: dict[str, list[tuple[int, str]]] = {}
    lines = sql.splitlines()
    current = ""
    depth = 0
    for idx, line in enumerate(lines, start=1):
        stripped = line.strip()
        if not current:
            m = re.match(
                r'CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))',
                stripped,
                re.IGNORECASE,
            )
            if not m:
                continue
            name = (m.group(1) or m.group(2)).lower()
            tables[name] = [(idx, line)]
            depth = stripped.count("(") - stripped.count(")")
            if depth > 0:
                current = name
            continue
        tables[current].append((idx, line))
        depth += stripped.count("(") - stripped.count(")")
        if depth <= 0:
            current = ""
    return tables


def strip_sql_comment(line: str) -> str:
    """去掉 SQL 行注释（`-- ...`），避免注释里的说明被当成列定义。"""
    idx = line.find("--")
    return line if idx < 0 else line[:idx]


def strip_go_comment(line: str) -> str:
    """去掉 Go 行注释。注释里提 `os/exec` 是在解释**为什么**不用它，不该判红。"""
    idx = line.find("//")
    return line if idx < 0 else line[:idx]


def check_a2(sql: str) -> list[str]:
    errors = []
    for idx, line in enumerate(sql.splitlines(), start=1):
        if A2_KEYWORDS.search(strip_sql_comment(line)):
            errors.append(
                f"A2 (I1: 中枢不存「怎么执行」): schema.sql:{idx} 出现可执行概念 → {line.strip()}"
            )
    return errors


def check_a4(tables: dict[str, list[tuple[int, str]]]) -> list[str]:
    errors = []
    for table in KIND_FORBIDDEN_TABLES:
        for idx, line in tables.get(table, []):
            if re.match(r"^\s*kind\s+\w", strip_sql_comment(line), re.IGNORECASE):
                errors.append(
                    f"A4 (I3: kind 不落库): {table} 表里出现 kind 列 → schema.sql:{idx}: {line.strip()}"
                )
    return errors


def central_packages() -> list[Path]:
    """中枢侧的一切包 = `internal/` 下的每个目录。

    动态枚举而不是手写清单：手写清单的失效方式正是 v3 的样子——`internal/executor`
    删了，清单还在，门禁恒红。动态枚举让「新加的包」自动进入管辖范围。
    """
    internal = REPO_ROOT / "internal"
    return sorted(p for p in internal.iterdir() if p.is_dir())


def check_a5() -> list[str]:
    """中枢侧包不得 import os/exec。"""
    errors = []
    pattern = re.compile(r'(^|\s|")(os/exec)("|\s|$)')
    packages = central_packages()
    for pkg in packages:
        for path in sorted(pkg.rglob("*.go")):
            if path.name.endswith("_test.go"):
                continue  # 测试可以在进程内起子进程，不属于产品执行路径
            for idx, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
                if pattern.search(strip_go_comment(line)):
                    errors.append(
                        f"A5 (I1: 中枢不执行): {path.relative_to(REPO_ROOT)}:{idx} import 了 os/exec → {line.strip()}"
                    )
    return errors


def check_a6(tables: dict[str, list[tuple[int, str]]]) -> list[str]:
    """内容寻址表必须有内容标识列。"""
    errors = []
    for table, column in DIGEST_COLUMN_TABLES.items():
        cols = tables.get(table)
        if not cols:
            errors.append(f"A6 自检失败：解析不到表 {table}（解析器或 schema 变了）")
            continue
        has = any(
            re.match(
                r"^\s*" + re.escape(column) + r"\s+\w",
                strip_sql_comment(line),
                re.IGNORECASE,
            )
            for _, line in cols
        )
        if not has:
            errors.append(
                f"A6 (I1: 内容寻址): {table} 表缺少 {column} 列 —— 没有内容寻址就没有可审计性"
            )
    return errors


def check_a7() -> list[str]:
    """节点侧不得持有能力归属的写路径（归属只由中枢部署派生）。"""
    errors = []
    root = REPO_ROOT / NODE_SIDE_DIR
    if not root.is_dir():
        errors.append(f"A7 自检失败：找不到节点侧目录 {NODE_SIDE_DIR}")
        return errors
    for path in sorted(root.rglob("*.go")):
        for idx, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            code = strip_go_comment(line)
            for table in ATTRIBUTION_TABLES:
                if table in code:
                    errors.append(
                        f"A7 (I1: 归属只能由中枢部署派生): {path.relative_to(REPO_ROOT)}:{idx} "
                        f"提到归属表 {table} → {line.strip()}"
                    )
    return errors


def check_a8() -> list[str]:
    """A1 上传授权/registry 边界。

    两条机械判据：
      1. 上传路由**不得**被会话中间件包裹（调用方是节点，手里只有 node token）；
      2. 中枢 handler 不得直接写产物 registry 表（只经 store 的授权事务）。
    """
    errors = []
    mux = REPO_ROOT / CENTRAL_SERVER_DIR / "mux.go"
    if not mux.is_file():
        errors.append("A8 自检失败：找不到 internal/server/mux.go")
        return errors
    lines = mux.read_text(encoding="utf-8").splitlines()
    # 路由的 pattern 是常量拼接（`"PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+…`），
    # 所以按共享常量定位，并取「注册语句窗口」（该行 + 后两行）作为判据：
    # handler 写在下一行是合法排版，不能只盯一行。
    anchors = [i for i, line in enumerate(lines) if UPLOAD_ROUTE_MARKER in strip_go_comment(line)]
    if not anchors:
        errors.append(
            f"A8 自检失败：mux.go 里找不到产物上传路由（{UPLOAD_ROUTE_MARKER}）—— "
            "路由没了就说明门禁盯错了地方"
        )
    else:
        joined = "\n".join("\n".join(lines[i : i + 3]) for i in anchors)
        if ".session(" in joined or "sessMW(" in joined:
            errors.append(
                "A8 (A1: 上传鉴权): 产物上传路由被会话中间件包裹 —— 调用方是节点，"
                "只有 node token（会话包裹会让真实节点请求 401）"
            )
        if "newArtifactUploadHandler" not in joined:
            errors.append(
                "A8 (A1: 上传鉴权): 上传路由没有接到 artifactUploadHandler —— "
                "授权点消失了"
            )
    server_dir = REPO_ROOT / CENTRAL_SERVER_DIR
    for path in sorted(server_dir.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for idx, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            code = strip_go_comment(line)
            for table in ARTIFACT_TABLES:
                if re.search(r"INSERT\s+INTO\s+" + table, code, re.IGNORECASE):
                    errors.append(
                        f"A8 (A1: registry 边界): {path.relative_to(REPO_ROOT)}:{idx} "
                        f"直接写 {table} —— registry 只能经 store 的授权事务登记"
                    )
    return errors


def check_a9() -> list[str]:
    """exact-five：五份 spec 必须在，旧单文件必须不在。"""
    errors = []
    if not API_DIR.is_dir():
        errors.append(f"A9 自检失败：找不到 {API_DIR}")
        return errors
    present = {p.name for p in API_DIR.glob("*.yaml")}
    for name in SPEC_FILES:
        if name not in present:
            errors.append(f"A9 (exact-five): api/{name} 缺失")
    if LEGACY_SPEC in present:
        errors.append(
            f"A9 (exact-five): api/{LEGACY_SPEC} 仍是 v3 单文件真源 —— 消费者会回退到它，请删除"
        )
    extra = sorted(present - set(SPEC_FILES) - {LEGACY_SPEC})
    if extra:
        errors.append(f"A9 (exact-five): api/ 下出现第六个 spec 文件 {extra}")
    return errors


def run_envelope_check() -> int:
    """串接 check_envelope.py（信封字段级规则）。

    用 importlib 按路径加载：两个脚本都在 `scripts/`，不是包，`import check_envelope`
    在 `python3 scripts/check_contract.py` 的 sys.path 下不可靠。
    """
    path = Path(__file__).resolve().parent / "check_envelope.py"
    if not path.is_file():
        print(f"❌ A10 自检失败：找不到 {path}", file=sys.stderr)
        return 1
    spec = importlib.util.spec_from_file_location("check_envelope", path)
    if spec is None or spec.loader is None:
        print(f"❌ A10 自检失败：无法加载 {path}", file=sys.stderr)
        return 1
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return int(module.main())


def main() -> int:
    if not SCHEMA.exists():
        print(f"❌ 找不到 {SCHEMA}", file=sys.stderr)
        return 1

    sql = SCHEMA.read_text(encoding="utf-8")
    tables = parse_tables(sql)

    errors = check_a2(sql) + check_a4(tables) + check_a5() + check_a6(tables) + check_a7() + check_a8() + check_a9()

    # 自检：解析必须真的找到表，否则「没报错」毫无意义。
    for table in KIND_FORBIDDEN_TABLES:
        if table not in tables:
            errors.append(f"自检失败：解析不到表 {table}（解析器坏了，检查结果不可信）")
    if len(central_packages()) < 10:
        errors.append(
            f"自检失败：中枢侧只枚举到 {len(central_packages())} 个包 —— "
            "A5 的管辖范围被意外缩小了"
        )

    if errors:
        print("❌ 契约静态断言失败：", file=sys.stderr)
        for err in errors:
            print("  - " + err, file=sys.stderr)
        return 1

    print(
        f"  A2/A4/A5/A6/A7/A8/A9 通过（{len(tables)} 张表；{len(KIND_FORBIDDEN_TABLES)} 张 kind 禁令、"
        f"{len(DIGEST_COLUMN_TABLES)} 张内容标识、{len(central_packages())} 个中枢包、"
        f"{len(SPEC_FILES)} 份 spec）"
    )
    return run_envelope_check()


if __name__ == "__main__":
    sys.exit(main())
