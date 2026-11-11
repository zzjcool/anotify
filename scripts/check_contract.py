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

  A2 (I1)  中枢不持有「怎么执行」：schema 里不得出现 argv/shell/script
  A4 (I3)  kind 不落库：能力/节点/边三张表不得有 kind 列（引号标识符也算）
  A5 (I1)  中枢侧**一切包**不得 import os/exec：`internal/*` 每个子目录 + `cmd/server`
  A6 (I1)  内容寻址：能力行/部署/产物两表都必须有内容标识列（引号标识符也算）
  A7 (I1)  节点不得持有能力归属写路径：`cmd/anode/**` 不得出现归属表名
  A8 (A1)  上传授权/registry 边界：上传端点必须是 node token（不是会话）且接
           artifactUploadHandler；中枢 handler 不得直接写 `task_artifact_*`
  A9       五份 spec 的**存在性**：`api/` 下恰好五份、旧单文件已删除
  A11      parity 门禁必须真的存在（防 `go test -run` 无匹配 → exit 0 的假绿）

A8 为什么必须机械可查：A1 的授权语义（attempt/lease/attemptToken/相位/撤销/命名空间）
全在 store 的事务里复核，一旦有人为了「先收字节」把它挪到 handler 的裸 INSERT，行为
看起来一样、迟到文件却能落库（G3-D3 的硬语义）。A8 检查「谁在写 registry」。

**本脚本的 A 编号与 CONTRACT.md §9 的 A1–A12 同号不同义**：§9 那张表是**运行期**断言
（e2e 侧守），这张表是**静态**断言。CONTRACT.md §9.1 已把两套编号与关系写清。

信封字段级规则（status 只读、camelCase、`[]`、cursor、CAS、具名错误形状）由
`scripts/check_envelope.py` 负责，本脚本在末尾串接它——`make contract` 的 Makefile
配方属 G4 owner，本 issue 不改配方，所以由这里调用，而不是新增一个 Make 目标。
**两条腿各自独立报错**：envelope 检查不会因为上面某条红了就被跳过（否则信封门禁
会被一条无关的 A 编号错误掩埋）。

自测（`self_test`）在每次运行时都跑：在临时假仓库里逐条注入错误，断言每个 guard 抓得住。
检查器坏了要立刻红，而不是假绿。
"""

from __future__ import annotations

import importlib.util
import re
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

# A4 管辖的表：能力定义、节点、路由投影。这三张是「kind 应该由声明推导而不是存储」
# 的地方（契约 §5）。`sources.kind` 是**合法**的（源类型是用户给的一等字段），
# 所以必须按表名限定，不能全库 grep。
KIND_FORBIDDEN_TABLES = ("capability_versions", "nodes", "edges")

# A2 的关键词（不变量 I1：中枢不知道"怎么执行"，只知道"做什么"）。
#
# 与 Makefile 的 `grep -riE 'argv|shell|script'` 有一点差异：这里先剥 SQL 行注释，
# 因为「注释里说清楚为什么不存 argv」不该判红；Makefile 那一步是更严的原文扫描，
# 两步都跑（Makefile 在前），所以整体只会更严、不会更松。
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
# 上传路由的锚点：pattern 由共享常量拼出来（`"PUT "+protocol.NodeTaskArtifactsPathPrefix+…`），
# 所以按常量名定位，而不是靠路径字面量。
UPLOAD_ROUTE_ANCHOR = "NodeTaskArtifactsPathPrefix"
UPLOAD_HANDLER = "newArtifactUploadHandler"
# 「会话中间件」的判据：方法调用与常见别名变量名都算。
# 为什么不能只认 `.session(`：中间件常被赋给局部变量再包（`sessMW := d.session`），
# 只认方法名会漏掉整类别名——review 实测 `sessionMW(...)` 就能绕过。
MIDDLEWARE_DENYLIST = (".session(", "sessMW(", "sessionMW(", ".admin(", "adminMW(")

# A9：exact-five 的文件清单来自 `check_openapi.SPEC_FILES`（单一实现）。


# --------------------------------------------------------------------------------------
# SQL / Go 源码的轻量解析
# --------------------------------------------------------------------------------------

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


def strip_go_line_comment(line: str) -> str:
    """去掉 Go 的行注释，**但不碰字符串字面量里的 `//`**。

    为什么不能直接 `line.split("//")[0]`：`var u = "https://x/api/deployments"` 里的
    表名会被当成注释丢掉，A7 于是漏报（review 找到的真实绕过面）。这里做最小的
    字符串感知扫描：遇 `"`/`` ` `` 进入/退出字面量，只在字面量外认 `//`。
    """
    in_str = ""
    i = 0
    while i < len(line):
        ch = line[i]
        if in_str:
            if ch == "\\" and in_str == '"':
                i += 2
                continue
            if ch == in_str:
                in_str = ""
        elif ch in ('"', "`"):
            in_str = ch
        elif ch == "/" and i + 1 < len(line) and line[i + 1] == "/":
            return line[:i]
        i += 1
    return line


def statement_at(lines: list[str], index: int) -> str:
    """从某行起取出一个完整的 Go 语句（括号平衡即止）。

    为什么需要：注册语句会被 gofmt 折成多行（`mux.Handle(\\n pattern,\\n handler,\\n)`）。
    只取固定窗口（比如"该行 + 后两行"）会被纯排版改动打成假红，也会漏掉更远的 handler。
    """
    depth = 0
    parts: list[str] = []
    for line in lines[index:]:
        code = strip_go_line_comment(line)
        parts.append(code)
        depth += code.count("(") - code.count(")")
        if depth <= 0 and parts:
            break
    return "\n".join(parts)


def go_lines_without_comments(path: Path) -> list[str]:
    return [strip_go_line_comment(line) for line in path.read_text(encoding="utf-8").splitlines()]


def _load_openapi():
    """加载 `check_openapi.py`（A9 的 exact-five 与 spec 清单只有一份实现）。"""
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


def central_packages(root: Path) -> list[Path]:
    """中枢侧的一切包 = `internal/` 下每个子目录 + 中枢二进制 `cmd/server`。

    动态枚举而不是手写清单：手写清单的失效方式正是 v3 的样子——`internal/executor`
    删了，清单还在，门禁恒红。动态枚举让「新加的包」自动进入管辖范围。
    `cmd/server` 必须显式加上：中枢二进制本身也是「中枢侧」（review 找到的绕过面）。
    """
    packages = [p for p in (root / "internal").iterdir() if p.is_dir()]
    server_cmd = root / "cmd" / "server"
    if server_cmd.is_dir():
        packages.append(server_cmd)
    return sorted(packages)


# --------------------------------------------------------------------------------------
# 各条断言（都接受 root，便于自测在假仓库里注入错误）
# --------------------------------------------------------------------------------------

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
    # 列定义可以是裸标识符或引号标识符（`"kind" TEXT`），SQL 两者等价。
    kind_col = re.compile(r'^\s*"?(kind)"?\s+\w', re.IGNORECASE)
    for table in KIND_FORBIDDEN_TABLES:
        for idx, line in tables.get(table, []):
            if kind_col.match(strip_sql_comment(line)):
                errors.append(
                    f"A4 (I3: kind 不落库): {table} 表里出现 kind 列 → schema.sql:{idx}: {line.strip()}"
                )
    return errors


def check_a5(root: Path) -> list[str]:
    """中枢侧包不得 import os/exec。"""
    errors = []
    pattern = re.compile(r'(^|\s|")(os/exec)("|\s|$)')
    for pkg in central_packages(root):
        for path in sorted(pkg.rglob("*.go")):
            if path.name.endswith("_test.go"):
                continue  # 测试可以在进程内起子进程，不属于产品执行路径
            for idx, line in enumerate(go_lines_without_comments(path), start=1):
                if pattern.search(line):
                    errors.append(
                        f"A5 (I1: 中枢不执行): {path.relative_to(root)}:{idx} import 了 os/exec → {line.strip()}"
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
        pattern = re.compile(r'^\s*"?' + re.escape(column) + r'"?\s+\w', re.IGNORECASE)
        if not any(pattern.match(strip_sql_comment(line)) for _, line in cols):
            errors.append(
                f"A6 (I1: 内容寻址): {table} 表缺少 {column} 列 —— 没有内容寻址就没有可审计性"
            )
    return errors


def check_a7(root: Path) -> list[str]:
    """节点侧不得持有能力归属的写路径（归属只由中枢部署派生）。"""
    errors = []
    node_dir = root / NODE_SIDE_DIR
    if not node_dir.is_dir():
        errors.append(f"A7 自检失败：找不到节点侧目录 {NODE_SIDE_DIR}")
        return errors
    for path in sorted(node_dir.rglob("*.go")):
        for idx, line in enumerate(go_lines_without_comments(path), start=1):
            for table in ATTRIBUTION_TABLES:
                if table in line:
                    errors.append(
                        f"A7 (I1: 归属只能由中枢部署派生): {path.relative_to(root)}:{idx} "
                        f"提到归属表 {table} → {line.strip()}"
                    )
    return errors


def check_a8(root: Path) -> list[str]:
    """A1 上传授权/registry 边界。

    判据：
      1. 上传路由**不得**被会话中间件包裹（调用方是节点，手里只有 node token），
         必须接 `newArtifactUploadHandler`，且**只有一条**这样的注册；
      2. 中枢 handler 不得直接写产物 registry 表（跨行 SQL 也算）。
    """
    errors = []
    server_dir = root / CENTRAL_SERVER_DIR
    mux = server_dir / "mux.go"
    if not mux.is_file():
        errors.append("A8 自检失败：找不到 internal/server/mux.go")
        return errors

    lines = mux.read_text(encoding="utf-8").splitlines()
    code_lines = [strip_go_line_comment(line) for line in lines]
    # 注册语句从 `mux.Handle(`/`mux.HandleFunc(` 所在行开始，按括号平衡取出整条语句。
    # 用「语句起点」而不是「出现关键字的每一行」：一条多行注册里 handler 与常量各占一行，
    # 按行计数会把同一条语句数成两处注册（假红）。
    starts = [
        i
        for i, line in enumerate(code_lines)
        if "mux.Handle(" in line or "mux.HandleFunc(" in line
    ]
    statements = {i: statement_at(lines, i) for i in starts}
    registrations = [
        i
        for i, stmt in statements.items()
        if UPLOAD_ROUTE_ANCHOR in stmt or UPLOAD_HANDLER in stmt
    ]
    if not registrations:
        errors.append(
            f"A8 自检失败：mux.go 里找不到产物上传路由（锚点 {UPLOAD_ROUTE_ANCHOR} / "
            f"handler {UPLOAD_HANDLER}）—— 路由没了就说明门禁盯错了地方"
        )
    if len(registrations) > 1:
        errors.append(
            f"A8 (A1: 上传鉴权): mux.go 里有 {len(registrations)} 处产物上传相关注册 —— "
            "一条端点只能有一个注册点（多注册会 panic 或悄悄分叉鉴权）"
        )
    for i in registrations:
        stmt = statements[i]
        if UPLOAD_ROUTE_ANCHOR not in stmt:
            errors.append(
                f"A8 (A1: 上传鉴权): mux.go:{i + 1} 的注册没有使用共享路径常量 "
                f"{UPLOAD_ROUTE_ANCHOR} —— 硬编码路径会让节点与中枢的路径定义分叉"
            )
        if UPLOAD_HANDLER not in stmt:
            errors.append(
                f"A8 (A1: 上传鉴权): mux.go:{i + 1} 的上传路由没有接到 "
                f"{UPLOAD_HANDLER} —— 授权点消失了"
            )
        for marker in MIDDLEWARE_DENYLIST:
            if marker in stmt:
                errors.append(
                    "A8 (A1: 上传鉴权): 产物上传路由被会话/管理员中间件包裹 "
                    f"（{marker}）—— 调用方是节点，只有 node token"
                    "（会话包裹会让真实节点请求 401）"
                )
                break

    # 用「空白归一」后的全文匹配跨行 INSERT。
    for path in sorted(server_dir.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        text = strip_go_line_comment(" ".join(path.read_text(encoding="utf-8").split()))
        for table in ARTIFACT_TABLES:
            if re.search(r"INSERT\s+INTO\s+" + table, text, re.IGNORECASE):
                errors.append(
                    f"A8 (A1: registry 边界): {path.relative_to(root)} 直接写 {table} —— "
                    "registry 只能经 store 的授权事务登记"
                )
    return errors


def check_a9(root: Path) -> list[str]:
    """exact-five：五份 spec 必须在，旧单文件必须不在（`.yml` 也算）。

    实现在 `check_openapi.exact_five_errors`（单一实现）。这里再查一次是刻意的
    belt-and-braces：`make contract` 的两步各自独立，谁挂了都能说清是哪一步。
    """
    api_dir = root / "api"
    if not api_dir.is_dir():
        return [f"A9 自检失败：找不到 {api_dir}"]
    return [f"A9 (exact-five): {err}" for err in _load_openapi().exact_five_errors(api_dir)]


def check_a11(root: Path) -> list[str]:
    """parity 门禁必须**真的存在**（防 `[no tests to run]` 的假绿）。

    `make contract` 的最后一步是 `go test ./internal/server -run TestOpenAPIParity`。
    Go 的 `-run` 用的是正则：**匹配不到任何测试时 exit 0**。这个陷阱已经发生过一次
    （v3 的 `openapi_parity_test.go` 被删除后，`make ci` 在 schema 漂移的情况下保持
    绿色，见 docs/v4/g3a-test-inventory.md）。所以这里静态断言测试函数存在且形状正确
    （必须是 `func TestOpenAPIParityXxx(t *testing.T)`——只写 `func TestOpenAPIParityXxx() bool`
    的话 `-run` 匹配到了却不是测试，仍然是假绿）。
    """
    errors = []
    path = root / CENTRAL_SERVER_DIR / "openapi_parity_test.go"
    if not path.is_file():
        errors.append(
            "A11 (parity 门禁): internal/server/openapi_parity_test.go 不存在 —— "
            "`go test -run TestOpenAPIParity` 会因无匹配用例而 exit 0（假绿）"
        )
        return errors
    text = path.read_text(encoding="utf-8")
    names = re.findall(
        r"^func (TestOpenAPIParity[A-Za-z0-9_]*)\(\s*[a-zA-Z_][A-Za-z0-9_]*\s+\*testing\.T\s*\)",
        text,
        re.MULTILINE,
    )
    if not names:
        errors.append(
            "A11 (parity 门禁): openapi_parity_test.go 里没有 `func TestOpenAPIParity...(t *testing.T)` "
            "形状的用例 —— `-run TestOpenAPIParity` 可能匹配不到真测试"
        )
    return errors


def check_static(root: Path) -> list[str]:
    """全部静态断言（不含 envelope）。root 可指向假仓库（自测用）。"""
    schema = root / "internal" / "store" / "schema.sql"
    errors: list[str] = []
    if not schema.is_file():
        return [f"自检失败：找不到 {schema}"]
    sql = schema.read_text(encoding="utf-8")
    tables = parse_tables(sql)
    errors += check_a2(sql)
    errors += check_a4(tables)
    errors += check_a5(root)
    errors += check_a6(tables)
    errors += check_a7(root)
    errors += check_a8(root)
    errors += check_a9(root)
    errors += check_a11(root)
    # 自检：解析必须真的找到表，否则「没报错」毫无意义。
    for table in KIND_FORBIDDEN_TABLES:
        if table not in tables:
            errors.append(f"自检失败：解析不到表 {table}（解析器坏了，检查结果不可信）")
    if len(central_packages(root)) < 3:
        errors.append(
            f"自检失败：中枢侧只枚举到 {len(central_packages(root))} 个包 —— "
            "A5 的管辖范围被意外缩小了"
        )
    return errors


# --------------------------------------------------------------------------------------
# 自测：在临时假仓库里逐条注入错误
# --------------------------------------------------------------------------------------

GOOD_SCHEMA = """
CREATE TABLE IF NOT EXISTS capability_versions (
    id TEXT PRIMARY KEY,
    digest TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
    id TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS "edges" (
    from_ref TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployments (
    id TEXT PRIMARY KEY,
    digest TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_artifact_uploads (
    id TEXT PRIMARY KEY,
    sha256 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS task_artifact_manifests (
    id TEXT PRIMARY KEY,
    sha256 TEXT NOT NULL
);
"""

GOOD_MUX = """
func register(mux *http.ServeMux) {
	mux.Handle("GET /v1/things", noStore(h))
	mux.Handle("PUT "+protocol.NodeTaskArtifactsPathPrefix+"{taskId}"+
		protocol.NodeTaskArtifactsPathInfix+"{sha256}",
		noStore(newArtifactUploadHandler(db, spool, log)))
}
"""

GOOD_PARITY = """
package server

func TestOpenAPIParityFiveSpecs(t *testing.T) {}
"""



def build_fake_repo(root: Path) -> None:
    """造一个「全部断言都该绿」的最小假仓库。"""
    (root / "internal" / "store").mkdir(parents=True, exist_ok=True)
    (root / "internal" / "store" / "schema.sql").write_text(GOOD_SCHEMA, encoding="utf-8")
    (root / "internal" / "server").mkdir(parents=True, exist_ok=True)
    (root / "internal" / "server" / "mux.go").write_text(GOOD_MUX, encoding="utf-8")
    (root / "internal" / "server" / "openapi_parity_test.go").write_text(GOOD_PARITY, encoding="utf-8")
    (root / "internal" / "zz").mkdir(parents=True, exist_ok=True)
    (root / "internal" / "zz" / "pkg.go").write_text('package zz\n\nimport "fmt"\n', encoding="utf-8")
    (root / "cmd" / "server").mkdir(parents=True, exist_ok=True)
    (root / "cmd" / "server" / "main.go").write_text('package main\n\nimport "fmt"\n', encoding="utf-8")
    (root / "cmd" / "anode").mkdir(parents=True, exist_ok=True)
    (root / "cmd" / "anode" / "main.go").write_text('package main\n\nimport "fmt"\n', encoding="utf-8")
    (root / "api").mkdir(parents=True, exist_ok=True)
    for name in _load_openapi().SPEC_FILES.values():
        (root / "api" / name).write_text("openapi: 3.1.0\n", encoding="utf-8")


def _mutations() -> dict[str, tuple[str, callable]]:
    """变异表：名字 → (期望命中的错误子串, 变异函数(root))。

    每条变异都直接改**假仓库里的文件**，然后整跑 `check_static`——这样验的是
    「门禁在真实仓库形状上真的会红」，而不是孤立函数的返回值。
    """

    def a2_argv(root: Path) -> None:
        path = root / "internal" / "store" / "schema.sql"
        path.write_text(GOOD_SCHEMA + "CREATE TABLE t (script TEXT NOT NULL);\n", encoding="utf-8")

    def a4_kind_quoted(root: Path) -> None:
        path = root / "internal" / "store" / "schema.sql"
        path.write_text(
            GOOD_SCHEMA.replace(
                "CREATE TABLE IF NOT EXISTS nodes (\n    id TEXT PRIMARY KEY\n);",
                'CREATE TABLE IF NOT EXISTS nodes (\n    "kind" TEXT,\n    id TEXT PRIMARY KEY\n);',
            ),
            encoding="utf-8",
        )

    def a5_internal(root: Path) -> None:
        (root / "internal" / "zz" / "pkg.go").write_text(
            'package zz\n\nimport "os/exec"\n', encoding="utf-8"
        )

    def a5_cmd_server(root: Path) -> None:
        (root / "cmd" / "server" / "main.go").write_text(
            'package main\n\nimport "os/exec"\n', encoding="utf-8"
        )

    def a6_digest_renamed(root: Path) -> None:
        path = root / "internal" / "store" / "schema.sql"
        path.write_text(
            GOOD_SCHEMA.replace("    digest TEXT NOT NULL\n);", "    content_hash TEXT NOT NULL\n);", 1),
            encoding="utf-8",
        )

    def a7_attribution(root: Path) -> None:
        (root / "cmd" / "anode" / "main.go").write_text(
            'package main\n\nvar u = "https://x/api/deployments"\n', encoding="utf-8"
        )

    def a8_session(root: Path) -> None:
        path = root / "internal" / "server" / "mux.go"
        path.write_text(
            GOOD_MUX.replace(
                "noStore(newArtifactUploadHandler(db, spool, log))",
                "sessionMW(newArtifactUploadHandler(db, spool, log))",
            ),
            encoding="utf-8",
        )

    def a8_duplicate(root: Path) -> None:
        path = root / "internal" / "server" / "mux.go"
        path.write_text(
            GOOD_MUX.replace(
                "\n}\n",
                '\n\tmux.Handle("PUT /v1/x",\n\t\tnoStore(newArtifactUploadHandler(db, spool, log)))\n}\n',
            ),
            encoding="utf-8",
        )

    def a8_multiline_insert(root: Path) -> None:
        (root / "internal" / "server" / "bad.go").write_text(
            "package server\n\nfunc f() {\n\t_ = `INSERT\nINTO task_artifact_uploads (id) VALUES (1)`\n}\n",
            encoding="utf-8",
        )

    def a9_legacy_yml(root: Path) -> None:
        (root / "api" / "openapi.yml").write_text("openapi: 3.1.0\n", encoding="utf-8")

    def a9_sixth(root: Path) -> None:
        (root / "api" / "extra.yml").write_text("openapi: 3.1.0\n", encoding="utf-8")

    def a11_missing(root: Path) -> None:
        (root / "internal" / "server" / "openapi_parity_test.go").unlink()

    def a11_fake(root: Path) -> None:
        (root / "internal" / "server" / "openapi_parity_test.go").write_text(
            "package server\n\nfunc TestOpenAPIParityHelper() bool { return true }\n", encoding="utf-8"
        )

    return {
        "A2 schema 里出现 argv": ("A2", a2_argv),
        "A4 引号标识符 kind": ("A4", a4_kind_quoted),
        "A5 internal 包 import os/exec": ("A5", a5_internal),
        "A5 cmd/server import os/exec": ("A5", a5_cmd_server),
        "A6 内容标识列被改名": ("A6", a6_digest_renamed),
        "A7 节点侧出现归属表名": ("A7", a7_attribution),
        "A8 上传路由被会话包裹": ("中间件包裹", a8_session),
        "A8 重复上传路由": ("处产物上传相关注册", a8_duplicate),
        "A8 跨行 INSERT 产物表": ("registry 边界", a8_multiline_insert),
        "A9 旧单文件 openapi.yml": ("旧单文件真源已退役", a9_legacy_yml),
        "A9 第六个 spec extra.yml": ("第六个 spec 文件", a9_sixth),
        "A11 parity 测试缺失": ("A11", a11_missing),
        "A11 假测试函数形状": ("A11", a11_fake),
    }


def self_test() -> list[str]:
    """返回自测失败项（空 = 检查器本身可信）。

    每条变异都在**全新的假仓库**里跑：复用同一个目录会让上一条变异的残留文件
    （比如上一步加进去的 bad.go）污染下一条的结论。
    """
    failures: list[str] = []

    def run(mutate) -> list[str]:
        with tempfile.TemporaryDirectory(prefix="contract-selftest-") as tmp:
            root = Path(tmp)
            build_fake_repo(root)
            if mutate is not None:
                mutate(root)
            return check_static(root)

    baseline = run(None)
    if baseline:
        failures.append(f"自测基线应为 0 错误，实际 {len(baseline)} 条：{baseline[:3]}")
        return failures

    for label, (token, mutate) in _mutations().items():
        errors = run(mutate)
        if not any(token in err for err in errors):
            failures.append(f"自测 {label}: 没抓到（期望 {token!r}，实际 {errors[:2]}）")
    return failures


# --------------------------------------------------------------------------------------
# envelope 串接
# --------------------------------------------------------------------------------------

def run_envelope_check() -> int:
    """串接 check_envelope.py（信封字段级规则）。

    用 importlib 按路径加载：两个脚本都在 `scripts/`，不是包，`import check_envelope`
    在 `python3 scripts/check_contract.py` 的 sys.path 下不可靠。
    关掉字节码写入：门禁不该在只读校验里往工作树丢 `__pycache__`。
    """
    path = Path(__file__).resolve().parent / "check_envelope.py"
    if not path.is_file():
        print(f"❌ A10 自检失败：找不到 {path}", file=sys.stderr)
        return 1
    previous = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        spec = importlib.util.spec_from_file_location("check_envelope", path)
        if spec is None or spec.loader is None:
            print(f"❌ A10 自检失败：无法加载 {path}", file=sys.stderr)
            return 1
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    finally:
        sys.dont_write_bytecode = previous
    return int(module.main())


def main() -> int:
    errors = check_static(REPO_ROOT)
    for failure in self_test():
        errors.append(f"自测失败：{failure}（检查器本身坏了，绿了也不可信）")

    if errors:
        print("❌ 契约静态断言失败：", file=sys.stderr)
        for err in errors:
            print("  - " + err, file=sys.stderr)
    else:
        schema = REPO_ROOT / "internal" / "store" / "schema.sql"
        tables = parse_tables(schema.read_text(encoding="utf-8"))
        print(
            f"  A2/A4/A5/A6/A7/A8/A9/A11 通过（{len(tables)} 张表；"
            f"{len(KIND_FORBIDDEN_TABLES)} 张 kind 禁令、{len(DIGEST_COLUMN_TABLES)} 张内容标识、"
            f"{len(central_packages(REPO_ROOT))} 个中枢包、"
            f"{len(_load_openapi().SPEC_FILES)} 份 spec）"
        )

    # 信封是**独立的一条腿**：即使上面的静态断言红了，也要把信封结论报出来
    # （否则一条无关的 A 编号错误会把整个信封门禁掩埋掉）。
    envelope_exit = run_envelope_check()
    if errors or envelope_exit != 0:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
