# DONE T14-返工

按任务卡 `t14-rework.md` 完成两个问题的修复，已真实自测并提交。

## 修复清单

### 问题 1 · 字体/图标引用 404（已修复）

- 从 T13（`wt-fecore/web/`）复制资源进 `web/`：
  - `web/fonts/`（fonts.css + caveat/greatvibes/sacramento.woff2）
  - `web/assets/favicon.svg`
  - `web/tokens.css`
- 三页引用改为相对路径：`fonts/fonts.css`、`assets/favicon.svg`（原 `../public/...` 已删除）
- **验证**：`python3 -m http.server 5715` + playwright 桌面+移动双视口，三页**资源类 4xx = 0、requestfailed = 0**（仅 `/v1/*` API 预期降级 404，与 T13 验收基线一致）

### 问题 2 · 硬编码色值 → tokens 变量（已修复）

- 三页 `<link rel="stylesheet" href="tokens.css">`，删除内联 `:root` 中与 tokens 重复的 10 个变量定义（仅保留页面特有 `--panel`）
- `tokens.css` 新增「语义亮色阶 + 功能面板色」令牌（该文件本就是设计认可的唯一色彩来源）：
  `--success-300/--error-300/--warn-300`（徽章文字）、`--panel-overlay/--panel-modal/--code-bg/--code-text/--on-solid`（中性功能色）
- 替换映射（全部走变量）：
  - `#34d399→var(--success)`、`#fda4af→var(--error-300)`、`#6ee7b7→var(--success-300)`、`#fcd34d→var(--warn-300)`
  - `rgba(251,113,133,*)→color-mix(in srgb, var(--error) *)`、`rgba(52,211,153,0.12)→var(--success-soft)`、`rgba(251,191,36,0.12)→var(--warn-soft)`
  - docs.html 语法高亮/方法徽章/参数表全部令牌化（`.tk-key/.tk-str/.tk-num/.m-get/.m-post/.m-patch/.m-delete/.param-name/.req`）
  - 功能面板色：`#12121c→var(--panel-overlay)`、`#0b0b12→var(--panel-modal)`、`#0a0a11→var(--code-bg)`、`#d6d3e0→var(--code-text)`、`#0a0a10→var(--on-solid)`
- **验证**：`grep -rno "#[0-9a-fA-F]\{6\}" keys/security/docs.html` → **空（exit=1）**；剩余仅 `#fff`（与 T13 ui.css 主按钮白一致）与 `&#123;/&#125;` HTML 实体（非色值）

## 未改动（守范围）

- 页面结构/文案/交互/JS 逻辑均未动，仅「引用修复 + 色值令牌化」
- 侧栏 IA 与 T13 一致（工作台/集成/账户分组不变）

## 自测命令与结果

```
cd wt-feadmin && python3 -m http.server 5715 --directory web &
playwright（系统 Chrome）桌面 1280px + 移动 390px：
  keys.html     → 资源4xx=0 reqFail=0 溢出=0  JS错误=0（仅 /v1/keys 降级 404）
  security.html → 资源4xx=0 reqFail=0 溢出=0  JS错误=0（仅 /v1/auth/* 降级 404）
  docs.html     → 资源4xx=0 reqFail=0          JS错误=0
grep -rno "#[0-9a-fA-F]\{6\}" web/keys.html web/security.html web/docs.html → 空
```

截图：/tmp/t14-keys.html.png、/tmp/t14-security.html.png、/tmp/t14-docs.html.png（桌面）；/tmp/t14m-*.png（移动）

## Commit

```
a12704d fix(web): T14 字体引用修复 + 色值令牌化  (wt-feadmin 分支)
9 files changed, 146 insertions(+), 70 deletions(-)
```

## 遗留风险（如实上报）

1. **docs.html 移动端（390px）参数表轻微横向溢出 4 元素**（TABLE/TBODY/TR/SPAN，right≈629>390）：**修复前基线就存在**（基线 6 个、现 4 个，本次改动未引入反而略减），属页面本身的移动端适配问题，**不在本次返工范围**（任务卡明确「不要改结构」）。建议协调者另立移动端适配任务统一处理。
2. `#fff` 保留（白色主按钮/选中文字），与 T13 `ui.css` 完全一致——tokens.css 未定义纯白令牌，这是参考实现的既有约定。
3. 新增的中性面板令牌（`--panel-overlay` 等）目前只在 T14 的 tokens.css；**集成期建议协调者将其提升到 T13 的 tokens.css 统一**（T13 的 ui.css 里 #12121c/#0a0a10 等仍是裸值）。
