/* ============================================================
   ANOTIFY · 设计令牌 → JS 桥
   让 Chart.js 等脚本读取 tokens.css 里的同一份色值，
   保证 DOM 与图表永远一致（改 tokens.css 一处，全站生效）。
   用法：const c = tokens();  c.accent / c.success / ...
   ============================================================ */
function tokens() {
	const v = (name) =>
		getComputedStyle(document.documentElement).getPropertyValue(name).trim();
	return {
		// 中性
		bg: v("--bg"),
		text: v("--text"),
		muted: v("--muted"),
		faint: v("--faint"),
		line: v("--line"),
		lineStrong: v("--line-strong"),
		// 品牌
		accent: v("--accent"),
		accentStrong: v("--accent-strong"),
		accentSoft: v("--accent-soft"),
		accent1: v("--accent-1"),
		accent2: v("--accent-2"),
		accent3: v("--accent-3"),
		accent4: v("--accent-4"),
		// 语义
		success: v("--success"),
		error: v("--error"),
		warn: v("--warn"),
		info: v("--accent"),
		successSoft: v("--success-soft"),
		errorSoft: v("--error-soft"),
		warnSoft: v("--warn-soft"),
	};
}
