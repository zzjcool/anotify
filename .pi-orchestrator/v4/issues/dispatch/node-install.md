# v4.1 节点安装完整版（v3 对齐）· 冻结契约

- 状态：**冻结**（2026-11-12，root 裁决）
- 基线：`origin/v4/integration` @ `74a75a5`
- 分支：`feat/v4-node-install`（centurion 独占 worktree）
- 目标：恢复 v3「一条命令安装节点」体验，并保留 v4 的分工：`anode install` 只落盘，脚本负责服务生命周期。

## 0. 背景（已核实事实）

- v3：`scripts/install-agent.sh`（919 行）一条命令完成下载/校验/预检/systemd/启动/校验；UI 生成 `curl ... | sh -s -- --name --server --token`。
- v4 现状：`scripts/install-anode.sh` 仅下载+校验后转交 `anode install`；`anode install` 只写 bin/config/unit（不 sudo/systemctl）；UI 只显示静态 `anode install/serve/emit`。
- v4 中枢已有 `GET /v1/node/binary`（node token 鉴权，返回 `X-Agent-Version` / `X-Agent-Sha256`，未带二进制时 501）。
- v4 `anode serve` **没有**成功日志；错 token 时中枢 1008 close，`serve` 目前会无限重连（v3 是退出）。
- 本地 `make dev` 未导出 `ANOTIFY_AGENT_BIN` → `/v1/node/binary` 返回 501（e2e harness 会设）。

## 1. 冻结决策

| # | 决策 |
|---|---|
| D1 | **安装脚本仍是唯一安装入口**：`scripts/install-anode.sh` 负责下载/校验/预检/systemd/启停/卸载/升级；`anode install` 只落盘（保持"不 sudo/systemctl"边界）。 |
| D2 | **脚本下发**：新增 tracked 文件 `web/install-anode.sh`（与 `scripts/install-anode.sh` 字节相同），由静态服务匿名下发 `GET /install-anode.sh`（`web/` 已被静态路由覆盖，`.sh` 的 Content-Type 已有，**不加新路由、不改 OpenAPI**）。`check_anode_skill.py` 增加 `cmp` 守卫。 |
| D3 | **UI 一条命令**：注册节点成功后显示（可复制）：`curl -fsSL <origin>/install-anode.sh \| sh -s -- --name <name> --server <origin> --token <token>`。脚本内部完成 sudo 探测与提权（`curl\|sh` 下 sudo 从 `/dev/tty` 读密码），UI 命令**不**写 `sudo`。保留 `anode.yaml` 块与 `anode serve` 手工路径。 |
| D4 | **下载优先级**：`--download-url`（测试/内网）> `--version`（GitHub Release `anode_linux_<arch>` + `.sha256`）> 中枢 `GET <server>/v1/node/binary`（Bearer token，校验 `X-Agent-Sha256`，版本取 `X-Agent-Version`）。中枢 501 → 明确提示改用 `--version`；401 → 令牌无效。**校验和缺失/工具缺失一律失败**（不做 v3 的"假通过"降级）。 |
| D5 | **预检**：新增 `anode serve --check`（一次性 WS：hello → welcome → exit 0；close/401/超时 → 非 0 + 诊断）。脚本在停旧服务后、落配置前用临时 config 预检；`--no-preflight` 跳过。`serve` 本身同步修：握手被 1008 拒绝（close 早于 welcome）视为身份拒绝，退出而非无限重连。 |
| D6 | **systemd**：`--service auto\|system\|user\|none` + `--system/--user/--no-systemd`。auto：Linux+systemctl 且可提权→system；否则 user；无 systemctl→none。sudo：root 直跑 / `sudo -n` / tty 单次 `sudo -v`；`--no-sudo` 禁止。system 服务写 `User=<安装者>`（root 安装时取 `SUDO_USER`），并 `chown` config/data 给该用户；user 服务用 `systemctl --user` + linger（失败 warn）。启停后**轮询** `is-active`（最多 10s），不学前端的"立即判定"。`--no-systemd`/容器场景只落盘并打印手工命令。 |
| D7 | **unit 模板**：`anode install` 新增 `--unit-user`、`--unit-home`、`--unit-path`（可重复/单值）；模板在有值时输出 `User=`、`Environment=HOME=`、`Environment=PATH=`。v4 尚未发布，允许改模板（不做旧 unit 兼容）。 |
| D8 | **卸载/升级**：`--uninstall`（停/禁用/删 unit，删 bin；config 默认删、`--keep-config` 保留；data 仅 `--purge` 删）+ 结尾提示控制台删节点；重跑=升级（保留 config/token，`restart`+轮询 active）。 |
| D9 | **安全**：token 不进日志/报告；脚本用临时 `--token-file`（0600）传给 `anode install`，退出清理；不静默跳过 sha256；不执行远端 checksum 文件名。 |
| D10 | **release**：沿用 `.github/workflows/release.yml` 的 `anode_linux_{amd64,arm64}` + `.sha256`；本次不改 CI，只在文档写清"v4 未发版时用中枢下载"。 |

## 2. 文件归属（单 writer：centurion worktree）

- `scripts/install-anode.sh`（重写/扩展）
- `web/install-anode.sh`（D2 拷贝，字节相同；由脚本/`make` 校验）
- `scripts/check_anode_skill.py`（`cmp` 守卫 + 新 flag 同步）
- `cmd/anode/main.go`（`serve --check`；`install --unit-user/--unit-home/--unit-path`）
- `cmd/anode/internal/hub/client.go`（1008→身份拒绝；一次性 check）
- `cmd/anode/internal/install/install.go`（unit 模板 + 参数校验）
- `cmd/anode/internal/install/install_test.go`、`cmd/anode/internal/hub/client_test.go`、`cmd/anode/bootstrap_test.go`（安装脚本集成测试）
- `internal/server/static.go` 只在必要时（预期不动）
- `scripts/dev.sh`（导出 `ANOTIFY_AGENT_BIN="$ROOT/anode"`，让本地一键命令可用）
- `web-src/pages/nodes.html`（`#reg-bootstrap` 命令块 + 生成函数）
- `web-src/locales/{zh-CN,en,ja,es}.yaml`（新 key ×4）
- `docs/v4/anode-cli.md`（`--check`、新 install flags；check_anode_skill 会扫）
- `docs/anode-install.md`、`README.md`、`README.en.md`（一条命令 + 卸载/升级）
- `scripts/e2e/fixtures/f3/f3-node-retained.mjs`（`node-create` 新命令断言）
- `scripts/v4-frontend.test.mjs`（nodes 页单测，如涉及）

**禁止**：改 `main`/`v4/integration`、改 schema/API 契约、改 release.yml 的资产名、动 `/v1/node/binary` 语义、引入第三方依赖。

## 3. 脚本 CLI（冻结）

```
sh install-anode.sh [flags]
  --name NAME                 节点名（可选；已有 config 时沿用）
  --server URL                中枢（http(s):// 或 ws(s)://，脚本自行归一化）
  --token TOKEN | --token-file PATH
  --version V                 指定 GitHub Release 版本（可选；默认从中枢下载）
  --download-url URL          直接指定二进制 URL（测试/内网；可配 ANODE_DOWNLOAD_URL）
  --prefix DIR                传给 anode install
  --service auto|system|user|none   （默认 auto）
  --system | --user | --no-systemd
  --no-sudo
  --no-preflight
  --preflight-timeout SEC     （默认 20）
  --dry-run
  --uninstall [--purge] [--keep-config]
  -h|--help
```

环境变量（测试钩子/兼容）：`ANODE_VERSION`、`ANODE_DOWNLOAD_URL`、`ANODE_DOWNLOAD_BASE`、`ANODE_CHECKSUM_URL`、`ANODE_SUDO`、`ANODE_SYSTEMCTL`、`ANODE_LOGINCTL`、`ANODE_UNIT_PATH`、`ANODE_PREFLIGHT_SEC`。

退出码：0 成功（含卸载/帮助）；2 用法/下载/校验/预检/服务失败（统一非 0 即可，脚本内打印下一步）。

## 4. `anode serve --check` 契约

- `anode serve --check --config <path> --data <dir>`：不写盘、不监听、不启动 runner/listen/ops；只为验证身份与中枢可达。
- 成功：收到 `welcome` → stdout 一行 `check ok: node=<name>`（不含 token）→ exit 0。
- 失败：`anode check: <诊断>` 到 stderr，exit 2；诊断至少区分 `unauthorized`（close 1008/401）、`timeout`、`config`。
- 默认超时 10s（可 `--check-timeout`? 不新增 flag，固定 10s；脚本有外层 timeout）。

## 5. 验收（centurion 必须自跑）

Go：
- `go test ./cmd/anode/... ./internal/server/... -count=1`
- `make ci`（gofmt/vet/fe/contract/check-anode-skill/bench 冒烟）
- `make contract`
- `make check-anode-skill`

安装脚本集成（`cmd/anode/bootstrap_test.go` 扩展，普通用户可跑）：
- 正常安装：假 curl/假 systemctl/假 sudo/假 loginctl + 真 `anode` 二进制；断言下载、sha256、预检、install、daemon-reload/enable/restart 顺序、`is-active` 轮询、token 不在 stdout。
- 篡改 sha256 → 中止且未落盘。
- `--token` 与 `--token-file` 二选一缺失 → 非 0。
- `--no-preflight` 跳过；预检失败 → 非 0 且未装服务。
- `--service none` → 只落盘，不调用 systemctl。
- 重跑=升级：无 token/server 参数、config 保留、restart 调用。
- `--uninstall` / `--keep-config` / `--purge` / `--dry-run` 行为。
- 脚本与 `web/install-anode.sh` 字节一致（`cmp`）。

E2E：
- `node-create`（real）更新：`#reg-bootstrap` 含 `install-anode.sh`、`--name <name>`、`--server <origin>`、`--token <真令牌>`；复制按钮存在；截图前清空 token 的既有纪律不破。
- `live_agent`：`anode serve --check` 对错误 token 非 0（可在现有 suite 里加，不新增套件）。
- 全量 `make e2e` ×2 绿（28/28、≥2129 断言、TMP 零残留）。

## 6. 文案 / UI

- 新 i18n key（4 语言）：`nodes.v4.bootstrap_title`、`nodes.v4.bootstrap_hint`、`nodes.v4.copy_bootstrap`（可复用 `nodes.v4.copy`）。
- `#reg-bootstrap` 用现有 `.code-block/.code-head/.code-body/.copy-btn`，不新增类名。
- `node.html` / `docs.html` 的三行静态命令同步改为指向一条命令（避免漂移）。
- 命令里的 token 只进 `textContent`，不进 localStorage/console/URL；关闭面板即清空（复用 `clearToken`）。
- 不在截图里出现 token（既有 `f3-driver.mjs:823` 纪律）。

## 7. 非目标

- macOS/launchd 服务、Windows。
- OCI 拉取、自动注册节点、自动部署能力。
- 改 `/v1/node/binary` 协议/鉴权/头。
- 发布 v4 release（用户后续决定）；本次只保证 `--version` 路径与 release.yml 命名兼容。
- playbook/observed 相关任何改动。

## 8. 合并与门禁

- centurion 只在自己的 worktree 提交/推送 `feat/v4-node-install`，**不 merge、不 gh pr create/merge**。
- root 独立验收：脚本真跑、UI real e2e、全量门禁；reviewer ok:true 后由 root 合入 `v4/integration`（PR #367 会随之更新）。

## 9. 修订 R1（2026-11-12，rev-nodeinstall ok:false 后 root 裁决）

复审报告：`/root/tmp/nodeinstall-review.md`（1 critical + 4 major + 6 minor）。裁决如下：

- **C1（system 服务 chown 时序）**：修。`chown config/data` 必须在 `systemctl restart` **之前**（`anode install` + `enable` 之后）；`wait_active` 失败时先补偿（恢复旧服务）再 die。必须补一条**能红的**所有权敏感测试：假 `systemctl is-active` 以「config 属主 == unit 的 `User=`」为 active 条件。
- **M1（`--dry-run --service user` 调 loginctl）**：修。`DRY_RUN` 下不调用 `loginctl`/`systemctl`，只打印；补测试。
- **M2（`--token-file` 轮换不生效）**：修。`--token-file` 与 `--token` 同档置 `TOKEN_FROM_CLI=yes`；补「token-file 轮换 → 自动 `--update-token` → exit 0」测试。
- **M3（`--service none` 跳过预检）**：**改行为**——除 `--no-preflight` 外一律预检（含 `--service none`）；文档说明 `--check` 可能让旧实例瞬断（被 1008 顶掉后自愈）。补「错令牌 + service none → 非 0」测试。
- **M4（PATH 漂移导致升级失败）**：修。unit 的 `Environment=PATH` 收敛为**稳定集合**（`$HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`），不再照抄调用者 `$PATH`；显式 `--unit-path` 仍可覆盖。`anode install` 失败时若之前 stop 过服务，必须**恢复旧服务**再 die。补「不同 caller PATH 重跑 → 同一 unit、exit 0」测试。
- **minor**：修 `bootstrap_test.go:674` 尾随空格空断言；`live_agent.mjs` 错令牌断言必须只接受 `unauthorized`（不接受 timeout）；f3 `--token` 断言与 `issued.result.token` 做值等价并钉下载 host；失败用例判精确 exit 2（不是非 0）；保留「install 失败码传播」「`--server` 转发」覆盖；脚本下载令牌改用 `curl --config`（不进 argv）；`--prefix` + `--service system` 明确报错；文档同步（`--token` vs `--token-file`、`--check` 对在跑实例的影响）。
- **m6 扩展 flag**：契约 §3 的"冻结"指 `--help` 主表；测试/镜像钩子（`--arch/--checksum-url/--config/--bin/--unit/--data`）允许保留，文档 `usage` 里注明。
- **合并门禁**：修完后重跑 `go test ./cmd/anode/... ./internal/server/...`、`make ci`、`make contract`、`make check-anode-skill`、`make e2e` ×2；复审者按 §7 探针复跑（尤其 P17/P16b/V4/V2）。
