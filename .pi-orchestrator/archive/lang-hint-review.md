APPROVE

# 语言提示横幅（lang-hint）终审报告

> 终审人：anotify-reviewer ｜ 日期：2026-08-05 ｜ 分支：feat/lang-hint（未提交 diff vs main）
> 基准：lang-hint-requirements.md（含 §4.2/AC-4 协调者修订版）、lang-hint-design.md
> 验证：lang_hint 套件复审重跑 1/1 绿（52 断言）；sitegen 单测 ok；全量 12/12 绿采信协调者已验证事实

## 结论

**APPROVE，可合并。**

五项终审要点全部通过，红线零违反，设计稿逐条比对无偏离（仅一处合理的实现优化，见非阻塞项 1）。tester 报告的 i18n key 大小写 bug 已修复并经复审重跑确认（此前被阻塞的 AC-2.4/4.1/4.3/4.4 现全绿）。

## Blocking 问题

无。

## Non-blocking 问题 / 建议

1. **挂载时机与设计稿字面不同（合理偏离，无需改）**：设计 §5.3 写「DOMContentLoaded 自动执行」，实现为工作台页在 `mountLayout()` 末尾调用、login 页在 IIFE 加载时按 pathname 自动执行。partials.js 以裸 `<script src>` 在 body 尾部加载，DOM 已就绪；且该方式**更精确地满足了设计自身的关键约束**（必须在 mountLayout 之后 prepend 才能稳占 body 第一位）。设计稿措辞可日后随手更新，不阻塞。
2. **lang-hint-test-report.md 内容过时**：报告仍写「产品 bug 未修：4 条 AC 被阻塞」，实际修复后已全绿。建议 tester 日后在 bug 修复后同步刷新测试报告（流程 nit，已记入协调者的 EVOLUTION 待进化点同类项）。
3. **需求文档 §5.1 文案与设计 §6.2 文案不一致**（tester 已标注）：实现遵循设计稿（事实源），断言与设计一致。属文档间瑕疵，非产品问题。
4. **裸 `zh`（无区域码）行为依赖站点语言表顺序**：前缀匹配遍历 `Object.keys(lowerToCanon)`，`zh` 会命中第一个 primary 为 zh 的条目（当前仅 zh-CN，行为确定）。未来若加 zh-TW 站点语言，裸 `zh` 的落点取决于 alternate 链接顺序——届时需定义优先级。当前无影响，记录在案。

## 核验清单（全部通过）

- [x] **红线**：新代码无 `location.href` 自动执行/无 `location.assign/replace`（grep diff 确认，action 为用户点击的 `<a>`）；无 localStorage/sessionStorage/document.cookie 写入（grep diff 为空）；28 个构建期 HTML 零 `.lang-hint` 标记（grep 全部页面为空，横幅纯 JS 构建）→ 爬虫/无 JS 零影响。刷新重出横幅（AC-6.2）实测证明无持久化。
- [x] **检测逻辑**：`navigator.languages || [navigator.language]` 兜底；精确匹配优先于 primary-subtag 前缀匹配；两侧小写比较 + `lowerToCanon` 保规范大小写供 i18n key 查询（大小写修复正确）；zh-TW/zh-HK/zh-Hant/裸 zh → zh-CN 目标符合协调者修订版 AC-4；单语言构建（alternates 为空）静默返回；裸 key 守卫（解析值===key 则放弃）防 file:// 直开出裸 key；幂等检查防重复挂载；data-page 门控仅 index/login。
- [x] **i18n key**：12 key × 4 文件逐字节一致（值为目标语言文案，符合设计「让用户读得懂」决策）；运行时字典实测 `common.lang.hint.{text,action,dismiss}.zh-CN` 在 4 个 i18n.*.js 全部正确解析。
- [x] **设计契合**：流内推挤（max-height 过渡、无 fixed/absolute/z-index）；DOM 结构/class 命名/aria（role=region + aria-label、inner lang、action hreflang+lang、close aria-label、Esc+焦点归还、focus-visible outline）逐条对得上 §5/§8；动画 + prefers-reduced-motion 降级 + print 隐藏对得上 §7；移动端折行不压缩 action/close 对得上 §9；**零硬编码色值**（全部 tokens：--bg-raise/--line/--accent*/--text/--muted，grep 无 hex/rgb）。
- [x] **测试质量**：734 行套件，Playwright `newContext({locale})` 真实模拟浏览器语言（非 mock）；断言含目标语言文案内容（非仅元素存在）、query+hash 保留、刷新重出（无存储的实证）、构建 HTML 零横幅、移动端溢出、首 Tab 焦点序——语义真实覆盖 AC，无走过场/弱化断言痕迹。

## 范围外确认

- run_all.sh 仅追加 `lang_hint` 到 SUITES，无其他改动。
- 无暂存（staged）文件；改动仅限任务列明范围 + web/ 重新生成产物（28 页 data-page/hreflang 行变化，sitegen 再生成属预期）。
- 未改代码、未 commit（本职责只读）。
