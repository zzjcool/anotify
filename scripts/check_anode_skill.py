#!/usr/bin/env python3
"""#334 的节点 CLI 防漂移守卫：`docs/v4/anode-cli.md` ↔ **真 anode 二进制**。

## 为什么是「等价迁移」而不是「删掉 guard」

v3 的 `scripts/check_agent_skill.py` 守的是 `cmd/agent/skill.md`（内嵌手册）↔ 真二进制的
`-h` / `--skill` / `--version`。v4 的节点产品换成 `anode`（`serve|install|emit` 三个子命令，
没有内嵌手册），于是同一件事换了载体，守卫的**覆盖面只增不减**：

| v3 断言（check_agent_skill） | v4 等价断言（本文件） |
| --- | --- |
| 手册里每个 `anotify-agent <sub>` 是真子命令 | 手册代码块里每个 `anode <sub>` ∈ `{serve,install,emit}`；v3 子命令出现即红 |
| 手册里每个 `-flag` 在同一子命令的 FlagSet / 全局 pool 里 | 手册代码块里每个 `-flag` ∈ 那个子命令的 `-h` 输出（或全局 `-h/--help/--version`） |
| 手册必须覆盖全部子命令 | 三个子命令必须都出现在手册代码块里 |
| `--version` 一行且与注入值一致 | `--version` 恰好一行 JSON，`version`/`commit` = 注入值（label 形状＝Makefile 的 VERSION_LABEL） |
| `--skill` 首行与 `--version` 同一份版本串 | `scripts/install-anode.sh` 的版本校验必须吃得下真输出（允许 `+<commit>` 后缀） |
| v3 只有一个安装入口 `scripts/install-agent.sh` | `scripts/install-anode.sh` 是**完整安装器**（下载/校验/预检/systemd 生命周期/卸载/升级），且静态服务下发的 `web/install-anode.sh` 与它**字节相同**（UI 那条 `curl … | sh` 命令跑的就是被这里校验过的那份） |
| `cmd/agent` 不碰 node_capabilities（归属只由部署派生） | `cmd/anode` 只依赖 `internal/protocol` + `internal/artifact`，不 import 中枢包、不直连数据库；中枢也不 import `cmd/anode` |

动态部分一律跑**真二进制**（`-h` / `--version`），不 grep Go 源码：flag 的注册形状会重构，
`-h` 才是用户和脚本看到的那份事实。

单独跑：`make check-anode-skill`（也是 `make ci` 的一道门）。
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
MANUAL = REPO_ROOT / "docs" / "v4" / "anode-cli.md"
INSTALLER = REPO_ROOT / "scripts" / "install-anode.sh"
# `curl -fsSL <origin>/install-anode.sh` 下来喂给 `sh -s --` 的就是这一份（web/ 被静态路由覆盖）。
# 两份字节不一致 = 用户跑的脚本和仓库里被校验的脚本不是同一个——这类漂移没有任何
# 运行时错误可依赖，只能在这里当场红。
WEB_INSTALLER = REPO_ROOT / "web" / "install-anode.sh"
ANODE_PKG = "./cmd/anode"
SERVER_PKG = "./cmd/server"
MODULE = "github.com/zzjcool/anotify"

# 节点只有这三个子命令（顺序 = `anode -h` 的用法行顺序）。加子命令时这里也要加：
# 漏了就等于「手册不提它」，而那正是这类漂移守卫要消灭的状态。
EXPECTED_SUBS = ["serve", "install", "emit"]

# v3 的 anotify-agent 子命令：出现在 v4 的手册/代码里就是没清干净的旧面
# （`emit` 两代都有，不算退役）。
RETIRED_SUBS = ["run", "check", "schema", "upgrade", "runs", "show",
                "capabilities", "status"]

# 子命令的**关键** flag：`-h` 还在但少了这些 = 能力被删了却仍报「正常」，必须红。
# 其余 flag 由手册逐个对账（手册提到就必须存在），这里只钉住不可缺的那几个。
REQUIRED_FLAGS = {
    "serve": {"-config", "-data", "-check"},
    "install": {"-prefix", "-config", "-bin", "-unit", "-data", "-name", "-server",
                "-token", "-token-file", "-update-token",
                # 系统服务以安装者身份跑（unit 的 User=/Environment=HOME=/PATH=）
                "-unit-user", "-unit-home", "-unit-path"},
    "emit": {"-to", "-id", "-data"},
}

# flag 包隐式提供的开关 + main() 手写的 --version（它们不在任何 FlagSet 里，但永远可用）。
IMPLICIT_GLOBAL_FLAGS = {"-h", "--help", "--version"}

# 与 Makefile 的 VERSION_LABEL 同形状：`<version>+<commit>`。故意不是 "dev"，
# 否则「注入没生效」也能蒙混过关。
TEST_VERSION = "9.9.9-check"
TEST_COMMIT = "deadbee"
TEST_LABEL = f"{TEST_VERSION}+{TEST_COMMIT}"

# 手册里对子命令的引用：`anode serve ...`（`anode.yaml` 这种没有空格，不会误匹配）。
SUB_CALL_RE = re.compile(r"\banode ([a-z][a-z0-9-]*)")
# flag 记号。边界刻意收紧：URL 里的 `wss://`、`<label>` 占位、JSON 里的连字符都不是 flag。
FLAG_RE = re.compile(r"(?<![\w<>/:-])(--?[A-Za-z][A-Za-z0-9-]*)(?![\w<])")
# `-h` / `<sub> -h` 的 PrintDefaults 形状：两空格缩进 + `-name`。
HELP_FLAG_RE = re.compile(r"^\s+(-[A-Za-z][A-Za-z0-9-]*)")


def fail(errors: list[str], title: str) -> int:
    print(f"❌ {title}", file=sys.stderr)
    for err in errors:
        print("  - " + err, file=sys.stderr)
    return 1


def run(cmd: list[str], cwd: Path | None = None, env: dict | None = None,
        timeout: int = 120) -> tuple[int, str, str]:
    """跑一条命令并返回 (退出码, stdout, stderr)；不做 shell 展开。"""
    proc = subprocess.run(
        cmd,
        cwd=None if cwd is None else str(cwd),
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    return proc.returncode, proc.stdout, proc.stderr


def build_anode(dest: Path) -> tuple[bool, str]:
    """用与 Makefile 同形状的注入值构建真二进制（不给 "dev"，否则测不出注入）。"""
    ldflags = f"-s -w -X main.version={TEST_LABEL} -X main.commit={TEST_COMMIT}"
    code, out, err = run(
        ["go", "build", "-trimpath", "-ldflags", ldflags, "-o", str(dest), ANODE_PKG],
        cwd=REPO_ROOT,
        timeout=300,
    )
    if code != 0:
        return False, f"go build 失败（code={code}）\n{err.strip() or out.strip()}"
    return True, ""


def clean_env() -> dict:
    """模拟「一台陌生机器」：清掉 ANOTIFY_* / 别的注入，只留 PATH 等系统变量。"""
    return {k: v for k, v in os.environ.items() if not k.startswith("ANOTIFY_")}


def parse_flags(help_text: str) -> set[str]:
    flags = set()
    for line in help_text.splitlines():
        m = HELP_FLAG_RE.match(line)
        if m:
            flags.add(m.group(1))
    return flags


def normalise_flag(raw: str) -> str:
    return "-" + raw.lstrip("-")


def check_dynamic(binary: Path, workdir: Path) -> tuple[list[str], dict[str, set[str]]]:
    """A1/A3：真二进制的 `-h` / `<sub> -h` / `--version` / 未知子命令。"""
    errors: list[str] = []
    env = clean_env()

    # ---- `-h`：三个子命令 + 退出 0 ----
    code, out, err = run([str(binary), "-h"], cwd=workdir, env=env, timeout=15)
    if code != 0:
        errors.append(f"`-h` 退出码 = {code}，want 0（stderr={err.strip()!r}）")
    usage = out + err
    for sub in EXPECTED_SUBS:
        if sub not in usage:
            errors.append(f"`-h` 的用法行没提子命令 {sub!r}：{usage.strip()!r}")

    # ---- 不带子命令：明确报错退出 2（不是「悄悄起守护进程」）----
    code, out, err = run([str(binary)], cwd=workdir, env=env, timeout=15)
    if code != 2:
        errors.append(f"不带子命令应退 2（用法错误），实际 {code}")

    # ---- 未知子命令：退 2 且说清期望 ----
    code, out, err = run([str(binary), "bogus"], cwd=workdir, env=env, timeout=15)
    if code != 2:
        errors.append(f"未知子命令应退 2，实际 {code}（stdout={out.strip()!r}）")
    if "serve" not in (out + err):
        errors.append("未知子命令的报错没说清可用子命令（应列出 serve/install/emit）")

    # ---- `-h` 里没有 v3 的子命令（有就是旧面没清干净）----
    for retired in RETIRED_SUBS:
        if f"Usage of {retired}" in usage:
            errors.append(f"`-h`/子命令帮助里出现 v3 子命令 {retired!r}")

    # ---- 每个子命令的 `-h`：退出 0、flag 非空、关键 flag 齐全 ----
    sub_flags: dict[str, set[str]] = {}
    for sub in EXPECTED_SUBS:
        code, sub_out, sub_err = run([str(binary), sub, "-h"], cwd=workdir, env=env, timeout=15)
        if code != 0:
            errors.append(f"`{sub} -h` 退出码 = {code}，want 0（stderr={sub_err.strip()!r}）")
        # emit 把 -h 写 stdout（flag.ErrHelp 支路），serve/install 写 stderr；两路都扫。
        sub_flags[sub] = parse_flags((sub_out or "") + "\n" + (sub_err or ""))
        if not sub_flags[sub]:
            errors.append(f"`{sub} -h` 解析不到任何 flag —— 解析器或命令本身坏了")
            continue
        missing = REQUIRED_FLAGS[sub] - sub_flags[sub]
        if missing:
            errors.append(f"`{sub} -h` 缺关键 flag {sorted(missing)}（现存：{sorted(sub_flags[sub])}）")

    # ---- `--version`：一行 JSON，值 = 注入值，不碰配置/网络 ----
    code, out, err = run([str(binary), "--version"], cwd=workdir, env=env, timeout=15)
    if code != 0:
        errors.append(f"`--version` 退出码 = {code}，want 0（stderr={err.strip()!r}）")
    if err.strip():
        errors.append(f"`--version` 不该往 stderr 写东西（读配置/连网的痕迹）→ {err.strip()!r}")
    lines = out.splitlines()
    if len(lines) != 1:
        errors.append(f"`--version` 必须只打一行，实际 {len(lines)} 行：{out!r}")
    if lines:
        try:
            info = json.loads(lines[0])
        except json.JSONDecodeError as exc:
            errors.append(f"`--version` 不是合法 JSON（install-anode.sh 靠它核对）：{exc}")
            info = {}
        if info.get("version") != TEST_LABEL:
            errors.append(
                f"`--version` 的 version = {info.get('version')!r}，want 注入的 {TEST_LABEL!r}"
            )
        if info.get("commit") != TEST_COMMIT:
            errors.append(
                f"`--version` 的 commit = {info.get('commit')!r}，want 注入的 {TEST_COMMIT!r}"
            )
        if f'"version":"{TEST_VERSION}' not in lines[0]:
            errors.append(
                f"`--version` 的输出里找不到发布版本前缀 `\"version\":\"{TEST_VERSION}`"
                "（install-anode.sh 按前缀核对发布版本）"
            )

    return errors, sub_flags


def iter_code_blocks(text: str) -> list[str]:
    """取出所有 ``` 围栏代码块的内容（手册的可执行面就写在这里）。"""
    blocks: list[str] = []
    current: list[str] | None = None
    for line in text.splitlines():
        if line.strip().startswith("```"):
            if current is None:
                current = []
            else:
                blocks.append("\n".join(current))
                current = None
            continue
        if current is not None:
            current.append(line)
    return blocks


def strip_comment(line: str) -> str:
    """去掉行尾注释（` #` 之后），避免把注释里的 `--flag` 当成真的用了它。"""
    if line.lstrip().startswith("#"):
        return ""
    idx = line.find(" #")
    return line[:idx] if idx >= 0 else line


def join_continuations(block: str) -> list[str]:
    """把 `\\` 续行拼回逻辑行：多行 install 命令的 flag 归属同一个子命令。"""
    logical: list[str] = []
    buf = ""
    for raw in block.splitlines():
        line = strip_comment(raw)
        if not line.strip():
            continue
        if line.rstrip().endswith("\\"):
            buf += line.rstrip()[:-1] + " "
            continue
        buf += line
        logical.append(buf.strip())
        buf = ""
    if buf.strip():
        logical.append(buf.strip())
    return logical


def check_manual(text: str, sub_flags: dict[str, set[str]]) -> list[str]:
    """A2：手册代码块里的子命令 / flag 必须在真 `-h` 里；三个子命令都要被覆盖。"""
    errors: list[str] = []
    blocks = iter_code_blocks(text)
    if not blocks:
        return ["docs/v4/anode-cli.md 里没有 fenced 代码块（手册的操作面必须可机检）"]

    referenced: set[str] = set()
    for block in blocks:
        for line in join_continuations(block):
            sub = None
            m = SUB_CALL_RE.search(line)
            if m:
                token = m.group(1)
                if token in RETIRED_SUBS and token not in EXPECTED_SUBS:
                    errors.append(
                        f"手册代码块出现 v3 子命令 `anode {token}`（v4 只有 {EXPECTED_SUBS}）：{line!r}"
                    )
                elif token in EXPECTED_SUBS:
                    sub = token
                    referenced.add(token)

            for flag_match in FLAG_RE.finditer(line):
                raw = flag_match.group(1)
                # 全局开关按**原样**对账：anode 只认 `--version` / `-h` / `--help`，
                # 把 `-version`（单横线）写进手册就是假的。
                if raw in IMPLICIT_GLOBAL_FLAGS:
                    continue
                flag = normalise_flag(raw)
                global_pool = {normalise_flag(g) for g in IMPLICIT_GLOBAL_FLAGS}
                pool = global_pool | (sub_flags.get(sub, set()) if sub else set())
                if flag not in pool:
                    where = "全局 flag" if sub is None else f"`{sub}` 的 -h"
                    errors.append(
                        f"手册代码块用了 `{raw}`，但它不在{where}里（可用：{sorted(pool)}）：{line!r}"
                    )

    missing = [s for s in EXPECTED_SUBS if s not in referenced]
    if missing:
        errors.append(f"手册代码块没有覆盖这些子命令：{missing}（必须覆盖 {EXPECTED_SUBS}）")
    return errors


def check_installer(binary: Path, workdir: Path) -> list[str]:
    """A4：安装脚本的版本校验必须吃得下真 `--version`（允许 `+<commit>` 后缀）。"""
    errors: list[str] = []
    if not INSTALLER.exists():
        return [f"找不到 {INSTALLER}（v4 的安装面是 scripts/install-anode.sh）"]
    script = INSTALLER.read_text(encoding="utf-8")
    for needle, why in (
        ("--version", "install-anode.sh 必须用真二进制核对版本（不能只信下载源）"),
        ('\'"version":"\'',
         "install-anode.sh 必须核对 --version 的 version 字段"),
        ("'+'*", "install-anode.sh 的版本校验必须接受 `+<commit>` 后缀"),
        ('install "$@"', "install-anode.sh 必须把安装参数原样交给 `anode install`（不在脚本里重写 flags）"),
    ):
        if needle not in script:
            errors.append(f"{why}（缺 {needle!r}）")

    # 真的把版本校验跑一遍：用同一份注入值的二进制输出喂给脚本里的匹配规则。
    code, out, _ = run([str(binary), "--version"], cwd=workdir, env=clean_env(), timeout=15)
    if code != 0 or not out.strip():
        errors.append("`anode --version` 跑不出输出，install-anode.sh 的版本核对无从谈起")
    else:
        line = out.splitlines()[0]
        # 发布版本（tag）必须能作为前缀命中：`"version":"<tag>"` 或 `"version":"<tag>+…"`。
        if f'"version":"{TEST_VERSION}"' not in line and f'"version":"{TEST_VERSION}+' not in line:
            errors.append(
                "`--version` 的 version 字段不是发布版本前缀（install-anode.sh 会拒装）："
                f"{line!r}"
            )
    return errors


def check_installer_copy() -> list[str]:
    """A6：下发副本必须与仓库里那份**字节相同**（`cmp` 守卫）。

    `web/install-anode.sh` 是静态服务匿名下发的那份（UI 的 `curl|sh` 一条命令），
    `scripts/install-anode.sh` 是仓库里被 review / 被本文件校验的那份。两份一旦分叉，
    用户在装的是一个没人校验过的脚本，而且失败现场看起来跟安装器无关。
    """
    errors: list[str] = []
    if not INSTALLER.exists():
        return [f"找不到 {INSTALLER}（v4 的安装面是 scripts/install-anode.sh）"]
    if not WEB_INSTALLER.exists():
        return [
            f"找不到 {WEB_INSTALLER}（`curl|sh` 一条命令下发的那份安装器，D2）"
            "——生成它：cp scripts/install-anode.sh web/install-anode.sh"
        ]
    if INSTALLER.read_bytes() != WEB_INSTALLER.read_bytes():
        errors.append(
            "scripts/install-anode.sh 与 web/install-anode.sh 字节不同"
            "（下发副本必须与仓库里被校验的那份一致）："
            "cp scripts/install-anode.sh web/install-anode.sh"
        )
    return errors


def check_boundary() -> list[str]:
    """A5：节点是独立产品——不 import 中枢包、不直连数据库；中枢也不反 import 节点。"""
    errors: list[str] = []
    env = clean_env()
    code, out, err = run(["go", "list", "-deps", ANODE_PKG], cwd=REPO_ROOT, env=env, timeout=180)
    if code != 0:
        return [f"`go list -deps {ANODE_PKG}` 失败（code={code}）：{err.strip() or out.strip()}"]

    deps = [line.strip() for line in out.splitlines() if line.strip()]
    allowed_central = {f"{MODULE}/internal/protocol", f"{MODULE}/internal/artifact"}
    central = {d for d in deps if d.startswith(f"{MODULE}/internal/")}
    extra = central - allowed_central
    if extra:
        errors.append(
            "cmd/anode 依赖了中枢包（节点只能依赖 protocol + artifact）："
            + ", ".join(sorted(extra))
        )
    for forbidden in ("database/sql", "modernc.org/sqlite"):
        if forbidden in deps:
            errors.append(f"cmd/anode 直连数据库（依赖 {forbidden}）——节点不写中枢的库")

    code, out, err = run(["go", "list", "-deps", SERVER_PKG], cwd=REPO_ROOT, env=env, timeout=180)
    if code != 0:
        errors.append(f"`go list -deps {SERVER_PKG}` 失败（code={code}）：{err.strip() or out.strip()}")
        return errors
    for line in out.splitlines():
        if line.strip().startswith(f"{MODULE}/cmd/anode"):
            errors.append("中枢 import 了节点产品 cmd/anode（两侧必须是独立产品）")
            break
    return errors


def main() -> int:
    if not MANUAL.exists():
        print(f"❌ 找不到 {MANUAL}", file=sys.stderr)
        return 1
    manual_text = MANUAL.read_text(encoding="utf-8")

    with tempfile.TemporaryDirectory(prefix="anode-skill-") as tmp:
        tmp_path = Path(tmp)
        binary = tmp_path / "anode"
        ok, err = build_anode(binary)
        if not ok:
            return fail([err], "anode 二进制构建失败，无法校验 CLI")
        # 空目录 + 清掉 ANOTIFY_*：这就是「节点拿到一台陌生机器」的场景。
        workdir = tmp_path / "empty"
        workdir.mkdir()
        dynamic_errors, sub_flags = check_dynamic(binary, workdir)
        installer_errors = check_installer(binary, workdir)

    manual_errors = check_manual(manual_text, sub_flags)
    boundary_errors = check_boundary()
    copy_errors = check_installer_copy()

    errors = (dynamic_errors + installer_errors + manual_errors
              + boundary_errors + copy_errors)
    if errors:
        return fail(errors, "anode CLI 可发现性校验失败（手册/安装脚本 ↔ 真二进制）")

    print(
        "  anode CLI 一致："
        f"{len(EXPECTED_SUBS)} 个子命令 {EXPECTED_SUBS}、"
        f"{len(sub_flags['install'])} 个 install flag、"
        f"版本 {TEST_LABEL}（--version JSON 与 install-anode.sh 的核对一致）；"
        "下发副本 web/install-anode.sh 与 scripts/install-anode.sh 字节一致；"
        "边界：仅依赖 internal/protocol + internal/artifact，不直连数据库"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
