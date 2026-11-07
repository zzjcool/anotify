#!/usr/bin/env python3
"""#317 的防漂移守卫：`cmd/agent/skill.md` ↔ **真实二进制的命令表**。

手册里写的子命令与 flag 一定会在半年后与代码漂移（手册是人写的，代码是改的）。
这个脚本是 `make ci` 的一部分，把两边对账：

A1（静态）`skill.md` 里每个 `anotify-agent <sub>` 必须是真实子命令；
A2（静态）每个 `-flag` 必须出现在**同一行那个子命令**的 FlagSet 里，或者是一个全局 flag；
A3（静态）首行必须是 `# anotify-agent manual ({{VERSION}})` —— 版本占位符而不是写死的版本
    （写死的那一个在下个版本就是谎言）；
A4（动态）用一个假版本构建 agent，`--version` 必须一行、且与注入值一致；
A5（动态）`--skill` 首行必须与 `--version` 是**同一份版本串**（占位符真的被替换了）；
A6（动态）`--skill` 不读配置、不连网、不落盘：空目录 + 清掉 ANOTIFY_* 环境变量 + 断网
    条件下退出 0 且打出完整五节。

为什么用**真二进制**而不是 grep Go 源码：flag 的注册形状（`fs.String/Bool/Int`、
子命令各自的 FlagSet）会被重构，而 `-h` 的输出就是用户/agent 实际看到的那份事实。
用真二进制对账，等于校验「手册承诺的东西在 `-h` 里真的找得到」。

故意写错一个 flag（比如手册里写 `--port`，而 emit 已经没有这个 flag）会让 `make ci` 红。
单独跑：`make check-agent-skill`。
"""

import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
SKILL = REPO_ROOT / "cmd" / "agent" / "skill.md"
AGENT_PKG = "./cmd/agent"
VERSION_PKG = "github.com/zzjcool/anotify/internal/version"

# 手册必须覆盖的全部子命令（顺序 = `-h` 命令表的顺序）。加子命令时这里也要加：
# 漏了就等于「手册不提它」，而那正是 #317 要消灭的状态（有命令、但 agent 不知道）。
EXPECTED_SUBS = ["run", "check", "schema", "emit", "upgrade", "runs", "show"]

# 注入用的假版本元数据：不是 "dev"，否则「替换没生效」也能蒙混过关。
TEST_VERSION = "9.9.9-check"
TEST_COMMIT = "deadbee"
TEST_BUILT_AT = "2020-01-02T03:04:05Z"

# 手册首行：`# anotify-agent manual (<label>)`，label 由占位符在运行期替换。
BANNER_RE = re.compile(r"^# anotify-agent manual \((?P<label>[^)]*)\)$")
# 手册里对子命令的引用：`anotify-agent run ...`（紧跟 `-flag` 的写法不算，见 FLAG_RE）。
SUB_CALL_RE = re.compile(r"anotify-agent ([a-z][a-z0-9-]*)")
# flag 记号。边界刻意收紧：`3-a1`（ref 里的连字符）、`-a<attempts>`（占位符里的连字符）
# 都不是 flag，不能因为长得像就要求它出现在 FlagSet 里。
FLAG_RE = re.compile(r"(?<![\w<>-])(--?[A-Za-z][A-Za-z0-9-]*)(?![\w<])")
# flag 包隐式提供的两个开关：它们不在任何 FlagSet 里，但永远可用。
IMPLICIT_GLOBAL_FLAGS = {"-h", "-help"}
# `-h` 输出里两段的起点（命令表 / flag 表）。
SUBS_HEADING = "子命令:"
GLOBAL_HEADING = "全局 flag"
HOOK_LINE = "Agent? 运行: anotify-agent --skill"
HELP_FLAG_RE = re.compile(r"^  (--?[A-Za-z][A-Za-z0-9-]*)")


def fail(errors: list[str], title: str) -> int:
    print(f"❌ {title}", file=sys.stderr)
    for err in errors:
        print("  - " + err, file=sys.stderr)
    return 1


def run(cmd: list[str], cwd: Path | None = None, env: dict | None = None, timeout: int = 60):
    """跑一条命令并返回 (退出码, stdout, stderr)。不做 shell 展开（避免引号陷阱）。"""
    proc = subprocess.run(
        cmd,
        cwd=None if cwd is None else str(cwd),
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    return proc.returncode, proc.stdout, proc.stderr


def build_agent(dest: Path) -> tuple[bool, str]:
    ldflags = (
        f"-s -w "
        f"-X {VERSION_PKG}.Version={TEST_VERSION} "
        f"-X {VERSION_PKG}.Commit={TEST_COMMIT} "
        f"-X {VERSION_PKG}.BuiltAt={TEST_BUILT_AT}"
    )
    code, out, err = run(
        ["go", "build", "-trimpath", "-ldflags", ldflags, "-o", str(dest), AGENT_PKG],
        cwd=REPO_ROOT,
        timeout=300,
    )
    if code != 0:
        return False, f"go build 失败（code={code}）\n{err.strip() or out.strip()}"
    return True, ""


def parse_subcommands(help_text: str) -> list[str]:
    """从 `-h` 输出里取出子命令表（`子命令:` 之后、每条两个空格缩进的那一列）。"""
    subs: list[str] = []
    in_section = False
    for line in help_text.splitlines():
        if line.strip() == SUBS_HEADING:
            in_section = True
            continue
        if not in_section:
            continue
        if line.startswith(GLOBAL_HEADING) or line.startswith(HOOK_LINE) or not line.strip():
            break
        m = re.match(r"^  ([a-z][a-z0-9-]*)(?: |$)", line)
        if m:
            subs.append(m.group(1))
    return subs


def parse_flags(help_text: str) -> set[str]:
    """从 `-h` 输出里取出 flag 名（flag 包的 PrintDefaults 形状：两空格 + `-name`）。"""
    flags = set()
    for line in help_text.splitlines():
        m = HELP_FLAG_RE.match(line)
        if m:
            flags.add("-" + m.group(1).lstrip("-"))
    return flags


def parse_global_flags(help_text: str) -> set[str]:
    """全局 flag 段（`全局 flag…` 到发现钩子之间）。"""
    chunk: list[str] = []
    collecting = False
    for line in help_text.splitlines():
        if line.startswith(GLOBAL_HEADING):
            collecting = True
            continue
        if collecting and line.startswith(HOOK_LINE):
            break
        if collecting:
            chunk.append(line)
    return parse_flags("\n".join(chunk))


def line_context(line: str, known_subs: set[str]) -> str | None:
    """这一行在讲哪个子命令（用于把 flag 归到对的 FlagSet）。

    两种写法都认：
      - `anotify-agent emit --to ...`（可抄的完整命令）
      - 表格里的裸命令名 `| `emit` | 5 | ...`（退出码表按命令分节）
    """
    m = SUB_CALL_RE.search(line)
    if m and m.group(1) in known_subs:
        return m.group(1)
    for name in sorted(known_subs):
        if f"`{name}`" in line:
            return name
    return None


def check_skill_static(skill_text: str, sub_flags: dict[str, set[str]], global_flags: set[str]) -> list[str]:
    errors: list[str] = []
    global_pool = set(global_flags) | IMPLICIT_GLOBAL_FLAGS

    lines = skill_text.splitlines()
    if not lines:
        return ["skill.md 是空的"]

    # A3：首行版本占位符（写死版本 = 下个版本就撒谎）。
    banner = BANNER_RE.match(lines[0])
    if not banner:
        errors.append(
            f"skill.md:1 不是 `# anotify-agent manual (<version>)` 形状 → {lines[0]!r}"
        )
    elif banner.group("label") != "{{VERSION}}":
        errors.append(
            "skill.md:1 必须用 {{VERSION}} 占位符（版本由 -ldflags 注入，写死的值下次就是错的）"
            f" → 现在是 {banner.group('label')!r}"
        )

    referenced_subs: set[str] = set()
    known_subs = set(sub_flags)
    for idx, line in enumerate(lines[1:], start=2):
        sub_call = SUB_CALL_RE.search(line)
        if sub_call:
            referenced_subs.add(sub_call.group(1))
            if sub_call.group(1) not in known_subs:
                errors.append(
                    f"skill.md:{idx} 提到未知子命令 `{sub_call.group(1)}`"
                    f"（-h 里只有 {sorted(known_subs)}）"
                )
        sub = line_context(line, known_subs)

        for flag_match in FLAG_RE.finditer(line):
            flag = "-" + flag_match.group(1).lstrip("-")
            pool = global_pool if sub is None else (sub_flags.get(sub, set()) | global_pool)
            if flag not in pool:
                where = "全局 flag" if sub is None else f"`{sub}` 的 FlagSet 或全局 flag"
                errors.append(
                    f"skill.md:{idx} 提到 `{flag_match.group(1)}`，但它不在{where}里"
                    f"（-h 里能用的：{sorted(pool)}）"
                )

    # 自检：手册必须真的提到全部子命令，否则「没报错」只说明手册没写命令。
    missing = [s for s in EXPECTED_SUBS if s not in referenced_subs]
    if missing:
        errors.append(f"skill.md 没有引用这些子命令：{missing}（手册要覆盖 {EXPECTED_SUBS}）")
    return errors


def check_binary(binary: Path, workdir: Path) -> tuple[list[str], dict[str, set[str]], set[str]]:
    """A4/A5/A6：真二进制的 --version / --skill / -h。

    环境刻意清掉 ANOTIFY_*（模拟「空目录、没有 node.yaml、没有令牌」）：
    手册与版本必须在那种机器上照样可用。

    返回 (错误列表, {子命令: flag 池}, 全局 flag 池)；后两个是静态检查的输入——
    用真二进制的 -h 输出当事实源，而不是 grep Go 源码（flag 的注册形状会重构）。
    """
    errors: list[str] = []
    env = {k: v for k, v in os.environ.items() if not k.startswith("ANOTIFY_")}

    # ---- --version：一行，且与注入值一致 ----
    code, out, err = run([str(binary), "--version"], cwd=workdir, env=env, timeout=15)
    if code != 0:
        errors.append(f"`--version` 退出码 = {code}，want 0（stderr={err.strip()!r}）")
    version_lines = out.splitlines()
    if len(version_lines) != 1:
        errors.append(f"`--version` 必须只打一行，实际 {len(version_lines)} 行：{out!r}")
    version = commit = ""
    if version_lines:
        m = re.match(r"^anotify-agent (\S+) \((\S+), (\S+)\)$", version_lines[0])
        if not m:
            errors.append(
                f"`--version` 形状必须是 `anotify-agent <version> (<commit>, <builtAt>)`，实际 {version_lines[0]!r}"
            )
        else:
            version, commit, built_at = m.groups()
            if version != TEST_VERSION:
                errors.append(f"`--version` 的版本 = {version!r}，want 注入的 {TEST_VERSION!r}")
            if commit != TEST_COMMIT:
                errors.append(f"`--version` 的 commit = {commit!r}，want 注入的 {TEST_COMMIT!r}")
            if built_at != TEST_BUILT_AT:
                errors.append(f"`--version` 的构建时间 = {built_at!r}，want 注入的 {TEST_BUILT_AT!r}")
    if err.strip():
        errors.append(f"`--version` 不该往 stderr 写东西（日志/配置读取的痕迹）→ {err.strip()!r}")

    # ---- --skill：退出 0、首行同版本串、五节齐全 ----
    code, out, err = run([str(binary), "--skill"], cwd=workdir, env=env, timeout=15)
    if code != 0:
        errors.append(f"`--skill` 退出码 = {code}，want 0（stderr={err.strip()!r}）")
    skill_lines = out.splitlines()
    if not skill_lines:
        errors.append("`--skill` 什么也没输出")
    else:
        if version and commit:
            want = f"# anotify-agent manual ({version} {commit})"
            if skill_lines[0] != want:
                errors.append(
                    f"`--skill` 首行 = {skill_lines[0]!r}，want {want!r}（与 --version 同一份版本串）"
                )
        if len(skill_lines) < 60:
            errors.append(f"`--skill` 只有 {len(skill_lines)} 行，手册像是被截断了")
        for heading in ("## 1.", "## 2.", "## 3.", "## 4.", "## 5."):
            if not any(line.startswith(heading) for line in skill_lines):
                errors.append(f"`--skill` 缺小节 {heading!r}（起节点/写能力/排障/退出码/不要做）")
        # 消歧段：agent ≠ 服务端 anotify ≠ herdr（issue 验收最后一条）。
        head = "\n".join(skill_lines[:15])
        for name in ("anotify-agent", "anotify", "herdr"):
            if name not in head:
                errors.append(f"`--skill` 开头 15 行没有区分 {name!r}（agent/服务端/herdr 要能分清）")

    # ---- -h：命令表与各子命令的 flag 表 ----
    code, out, err = run([str(binary), "-h"], cwd=workdir, env=env, timeout=15)
    if code != 0:
        errors.append(f"`-h` 退出码 = {code}，want 0（stderr={err.strip()!r}）")
    # flag 包把 usage 写到 stderr（-h 是「用户主动要帮助」，退出码仍是 0）。
    help_text = err if parse_subcommands(out) == [] else out

    subs = parse_subcommands(help_text)
    if subs != EXPECTED_SUBS:
        errors.append(f"`-h` 的命令表 = {subs}，want {EXPECTED_SUBS}（顺序与内容都要对上）")
    if HOOK_LINE not in help_text:
        errors.append(f"`-h` 末尾缺发现钩子 `{HOOK_LINE}`")
    if "常驻进程" not in help_text:
        errors.append("`-h` 没有说清「不带子命令 = 常驻进程」（误跑一次会占掉节点连接）")

    sub_flags: dict[str, set[str]] = {}
    for sub in subs:
        code, sub_out, sub_err = run([str(binary), sub, "-h"], cwd=workdir, env=env, timeout=15)
        if code != 0:
            errors.append(f"`{sub} -h` 退出码 = {code}，want 0（stderr={sub_err.strip()!r}）")
            continue
        # flag 包的 usage 写在 stderr（-h 是用户主动要帮助，退出码仍是 0）；
        # 两路都扫，免得因为某个子命令换过输出流就静默漏掉全部 flag。
        text = (sub_err or "") + "\n" + (sub_out or "")
        sub_flags[sub] = parse_flags(text)
        if not sub_flags[sub]:
            errors.append(f"`{sub} -h` 解析不到任何 flag —— 解析器坏了，后面的检查不可信")

    return errors, sub_flags, parse_global_flags(help_text)


def main() -> int:
    if not SKILL.exists():
        print(f"❌ 找不到 {SKILL}", file=sys.stderr)
        return 1
    skill_text = SKILL.read_text(encoding="utf-8")

    with tempfile.TemporaryDirectory(prefix="anotify-skill-") as tmp:
        tmp_path = Path(tmp)
        binary = tmp_path / "anotify-agent"
        ok, err = build_agent(binary)
        if not ok:
            return fail([err], "agent 二进制构建失败，无法校验 skill.md")
        # 空目录 + 清掉 ANOTIFY_*：这就是「agent 拿到一台陌生机器」的场景。
        workdir = tmp_path / "empty"
        workdir.mkdir()
        dynamic_errors, sub_flags, global_flags = check_binary(binary, workdir)

    static_errors = check_skill_static(skill_text, sub_flags, global_flags)
    errors = dynamic_errors + static_errors
    if errors:
        return fail(errors, "agent CLI 可发现性校验失败（skill.md ↔ -h）")

    print(
        "  agent CLI 一致："
        f"{len(sub_flags)} 个子命令 {sorted(sub_flags)}、"
        f"{len(global_flags)} 个全局 flag、"
        f"版本 {TEST_VERSION}（--version 与 --skill 首行同一份）"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
