#!/usr/bin/env python3
"""CONTRACT.md §9 的静态不变量断言（A2 / A4）。

由 `make contract` 调用，是 `make ci` 的前置门禁。

为什么用 Python 而不是 grep/awk：
  awk 解析 `CREATE TABLE IF NOT EXISTS <name> (` 会把 `IF` 或 `(` 当成表名，
  导致 A4 永远匹配不到（负向测试发现：插入 `kind TEXT` 后检查仍然"通过"）。
  按括号配对解析表定义才是可靠的。

A2（不变量 I1：无 argv/shell）：schema 里不得出现可执行的命令/脚本概念。
A4（不变量 I3：kind 不落库）：能力/节点/边的表定义里不得有 kind 列。

"kind" 这个列名在 api_keys 上是合法的（apikey/passkey 是**授权类型**，
不是能力在链上的角色）。所以 A4 必须按表名限定，不能全库 grep。
"""

import re
import sys
from pathlib import Path

SCHEMA = Path("internal/store/schema.sql")

# A4 管辖的表：能力定义、节点、出口表。
# 这三张是所有「kind 应该由声明推导而不是存储」的地方（契约 §5）。
KIND_FORBIDDEN_TABLES = {"capability_versions", "nodes", "edges", "node_capabilities"}

# A2 的关键词（不变量 I1：中枢不知道"怎么执行"，只知道"做什么"）。
A2_KEYWORDS = re.compile(r"argv|shell|script", re.IGNORECASE)


def parse_tables(sql: str) -> dict[str, list[tuple[int, str]]]:
    """解析出 {表名: [(行号, 该行内容)]}。

    按 `CREATE TABLE ... (` 开始、括号配对结束来切分，
    这样能正确处理 `IF NOT EXISTS`、跨行列定义与注释。
    """
    tables: dict[str, list[tuple[int, str]]] = {}
    lines = sql.splitlines()
    current: str = ""  # 空串 = 不在表定义内
    depth = 0

    for idx, line in enumerate(lines, start=1):
        stripped = line.strip()

        if not current:
            m = re.match(
                r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)",
                stripped,
                re.IGNORECASE,
            )
            if not m:
                continue
            name = m.group(1).lower()
            tables[name] = [(idx, line)]
            depth = stripped.count("(") - stripped.count(")")
            if depth > 0:
                current = name  # 只有确实进入表体才保持状态
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


def check_a2(sql: str) -> list[str]:
    errors = []
    for idx, line in enumerate(sql.splitlines(), start=1):
        code = strip_sql_comment(line)
        if A2_KEYWORDS.search(code):
            errors.append(
                f"A2 (I1: 无 argv/shell): schema.sql:{idx} 出现可执行概念 → {line.strip()}"
            )
    return errors


def check_a4(tables: dict[str, list[tuple[int, str]]]) -> list[str]:
    errors = []
    for table in sorted(KIND_FORBIDDEN_TABLES):
        for idx, line in tables.get(table, []):
            code = strip_sql_comment(line)
            # 列定义形如 "kind TEXT ..."（行首是列名）
            if re.match(r"^\s*kind\s+\w", code, re.IGNORECASE):
                errors.append(
                    f"A4 (I3: kind 不落库): {table} 表里出现 kind 列 → schema.sql:{idx}: {line.strip()}"
                )
    return errors


def main() -> int:
    if not SCHEMA.exists():
        print(f"❌ 找不到 {SCHEMA}", file=sys.stderr)
        return 1

    sql = SCHEMA.read_text(encoding="utf-8")
    tables = parse_tables(sql)

    errors = check_a2(sql) + check_a4(tables)

    # 自检：解析必须真的找到表，否则"没报错"毫无意义
    for table in sorted(KIND_FORBIDDEN_TABLES):
        if table not in tables:
            errors.append(f"自检失败：解析不到表 {table}（解析器坏了，检查结果不可信）")

    if errors:
        print("❌ 契约静态断言失败：", file=sys.stderr)
        for e in errors:
            print("  - " + e, file=sys.stderr)
        return 1

    print(f"  A2/A4 通过（解析到 {len(tables)} 张表，检查 {len(KIND_FORBIDDEN_TABLES)} 张）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
