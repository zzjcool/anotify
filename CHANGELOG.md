# Changelog

本项目遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## 未发布

### 节点（anode）

- 任务环境的 `PATH` 改为「**跟着服务用户的登录 shell 走，但只抓一次**」：安装脚本用
  `env -i HOME=<服务用户> "$SHELL" -lc` 抓一次，写进 `install.json`（与 anode.yaml 同目录），
  重跑一律复用记录 → 换终端/换 ssh 会话重跑仍然是同一份 unit（`重跑=升级` 不被打断）。
  新增 `--refresh-path`（重抓）/ `--no-capture-path`（退回老的固定集合）。
  修复：mise / nvm / asdf 装的 node 在任务里不可见，`pi` 这类 `#!/usr/bin/env node` 的 CLI
  会以 `env: 'node': No such file or directory` 失败（还被能力脚本误报成"没登录"）。
- 新增 `anode path show|add`：`show [--raw]` 看任务 PATH 基线（`--raw` 给脚本用），
  `add DIR...` 往记录里追加目录并只重写 unit 的 `Environment=PATH=` 行 —— 存量节点补工具链
  目录不再需要 `--uninstall` 重装（升级窗口不掉线）。守卫：unit 的 PATH 与记录不一致
  （手改过）就拒绝。
- `anode install` 的 unit 守卫放行"同一套安装只改 PATH"（`--refresh-path` / `anode path add`），
  身份/落点（`User=`/`HOME=`/`ExecStart`）改动仍然拒绝；老版本固定集合的 unit 可一次性迁移。

## v0.2.0 — 2026-11-03

UI v2、动效/加载态、字体自托管与时区修复的合并发布节点。

### UI

- 双主题（dark / light / auto）与可折叠侧栏（264px / 76px），主题与折叠状态持久化。
- 统一 token / 组件层：`.card`、`.btn`、`.stat`、`.list-row`、`.page-title`、`.tab` 等；亮色主题全站可读。
- 首页工作台改为「KPI + 热力图 + 事件流」：事件流升为主列表，KPI 改为事件口径。
- 全站 20 页面逐页迁移；docs 文档页重排（标题层级、目录、代码块、窄屏 TOC）。
- 品牌恢复旧版字标：`An` 方块 + Sacramento 手写体，不再裁切 `f/y` 下伸部。

### 动效与加载态

- 基础动效词汇：`--dur-fast/mid/toast/drawer/modal-out`、`--ease/--ease-out`；全部遵守 `prefers-reduced-motion`。
- 按压反馈、toast 入/退场、弹层入场与 140ms 退场、移动抽屉 240ms + 遮罩、主题翻转旧色遮罩、KPI 数值更新、列表 stagger、skeleton 合成层 shimmer。
- 七页加载态与错误态：events、admin、security、node、source、playbook、trace；骨架高度对齐内容，`aria-busy` 与数据/错误在同一同步块落地。
- CLS：node 0.285→0、source 0.133→0、playbook 0.178→0.0007、index 0.0038→0.0002；admin/security 错误态不再永久闪骨架或吞成空态。
- 跨文档 View Transitions：opt-in 内联进 layout `<head>`，200ms 根快照淡入；reduced-motion 下退回硬切。

### 可访问性

- 移动抽屉：`role="dialog"`、`aria-modal`、Tab/Shift+Tab 焦点陷阱、Esc 关闭、焦点回到菜单按钮；桌面侧栏不挂 dialog 角色。
- 统一弹层 `openModal/closeModal`，关闭先淡出再 `hidden`，重开清除退场态。
- 亮色执行状态色对比度抬到 ≥4.5:1（running/abandoned/lease_expired 实测 4.83–4.87）。
- 焦点环、toast 无障碍镜像、`lang` 提示条首帧即终态。

### 字体与私有化部署

- Inter（300–700）、Fraunces（340–420 + italic 400）、JetBrains Mono（400–500）自托管 woff2（latin / latin-ext）；删除 Google Fonts CDN。
- 首页首屏只拉 Inter（约 46.9 KiB），断网/私有化部署字体照常生效。

### 数据与 mock

- mock 对齐真后端：`POST /v1/auth/recovery/regenerate → { ok, recoveryCodes }`；种子 Key 均带 `enabled`；revoke 置 `enabled:false` 并返回 `{ ok: true }`。
- mock task id 改为确定性 `tsk_mock_N`：首页 → 任务详情跨刷新不再 404。
- `/v1/stats` 的「今日」与热力图日桶改用服务端本地时区；`/v1/admin/stats` 同步。
- Docker runtime 安装 `tzdata`，compose / `.env.compose.example` 支持 `TZ`（默认 `Etc/UTC`，例 `Asia/Shanghai`）；openapi 口径同步。

### 工程守卫

- `check-classes` 扩展扫描 `web/partials.js` 与页面内联脚本的 `class:` 字面量：625 HTML + 154 JS = 779 类；新增 `--self-test`（11/11）。
- 新增 `p2_guards` e2e（30 断言）：0 外部字体请求、`document.fonts.check`、抽屉 a11y、reduce token 归零、VT opt-in 与 `pagereveal`。
- 全量 e2e：**1866 断言 / 0 失败**；`go test ./...` 15 包全绿。

### 升级提示

- 默认镜像时区为 `Etc/UTC`；需要本地「今日」统计时在 compose `.env` 设置 `TZ`（如 `Asia/Shanghai`）并重启。
- 字体已随二进制/静态资源发布，不再需要访问外部字体 CDN。
- 旧深链 `index?msg=` 仍可用；VT 已启用，脚本跳转竞态由内联 opt-in 规避。

### 已知取舍

- stats 使用「服务端时区」；多时区用户需未来增加用户级时区。
- DST 切换日附近热力图日桶可能有 ±1h 边界偏差（代码注释已说明）。
- `check-classes` 尚未覆盖 `className =` / `setAttribute("class")` / `classList.*` 的完整字面量；`class:` 字面量已覆盖。
