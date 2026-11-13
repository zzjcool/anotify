#!/usr/bin/env node
/* Anotify · 图库访问守卫
 *
 * 作用：拦住页面「绕开唯一入口」去碰 defer 加载的图库。
 *
 * 背景：图渲染库（dagre → flograph → graphs → traceview）是 defer 脚本，页面在
 * fetch 续体里画图时它可能还没执行完（TRACE-STARTUP：响应 132ms、DCL 152ms；
 * SOURCE-STARTUP 同构）。唯一正确入口是 Anotify.graphLib()（partials.js 里解析期
 * 挂的 DCL 门闩）。裸读 window.AnotifyGraphs 会把竞态直接放回页面，而且平时
 * 看不出来（API 慢一点就「恰好正常」）。
 *
 * 拦两种写法：
 *   ① 页面模板里出现 AnotifyGraphs（注释里提到不算：先剥 HTML 注释）
 *      → 应改成 `const G = await A.graphLib(文案);`
 *   ② 页面调了 graphLib( 却没有在解析期调 expectGraphics()
 *      → graphLib 必然抛 graph_barrier_missing（页面只剩错误态）
 *
 * 用法：
 *   node scripts/check-graph-access.mjs [--src=web-src] [--self-test]
 *   --self-test  跑脚本自身的红/绿自测（纯内存 fixture，不改仓库文件）
 *   默认在违规时列出文件与行号并以非零退出码失败（供 CI / make fe 拦下）
 */
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(
	process.argv.find((a) => a.startsWith("--src="))?.split("=")[1] ?? "web-src",
);

/** 剥掉 HTML 注释：注释里解释这个 bug 不算违规（`<!-- ... AnotifyGraphs ... -->`）。 */
function stripHtmlComments(html) {
	return html.replace(/<!--[\s\S]*?-->/g, (m) => m.replace(/[^\n]/g, ""));
}

/** 行号：在原文（保留行结构）里定位 needle。 */
function lineOf(text, index) {
	return text.slice(0, index).split("\n").length;
}

/**
 * 扫一份页面模板，返回违规列表 [{ line, rule, message }]。
 * 纯函数（无 fs、无全局状态），自测直接喂 fixture。
 */
export function scanTemplate(name, source) {
	const out = [];
	const text = stripHtmlComments(source);

	const lib = /\bAnotifyGraphs\b/.exec(text);
	if (lib) {
		out.push({
			file: name,
			line: lineOf(text, lib.index),
			rule: "no-direct-graph-lib",
			message:
				"页面不得直接引用 AnotifyGraphs（defer 图库还没执行时读到 undefined）—— 改用 `const G = await A.graphLib(文案);`",
		});
	}

	const usesLib = /\bgraphLib\s*\(/.test(text);
	const armsGate = /\bexpectGraphics\s*\(/.test(text);
	if (usesLib && !armsGate) {
		const call = /\bgraphLib\s*\(/.exec(text);
		out.push({
			file: name,
			line: lineOf(text, call.index),
			rule: "missing-expect-graphics",
			message:
				"调用了 graphLib() 但没在解析期调 expectGraphics()：门闩没挂上，graphLib 会抛 graph_barrier_missing —— 在页面脚本顶部加一行 `A.expectGraphics();`",
		});
	}
	return out;
}

async function htmlFiles() {
	const files = [];
	for (const dir of ["pages", "layouts"]) {
		const full = path.join(SRC, dir);
		let names = [];
		try {
			names = await fs.readdir(full);
		} catch {
			continue; // 目录不存在（裁剪过的树）不算失败
		}
		for (const n of names.filter((x) => x.endsWith(".html")).sort()) {
			files.push(path.join(full, n));
		}
	}
	return files;
}

async function main() {
	const files = await htmlFiles();
	const violations = [];
	for (const file of files) {
		const src = await fs.readFile(file, "utf8");
		violations.push(...scanTemplate(path.relative(SRC, file), src));
	}
	if (violations.length) {
		console.error(`❌ 图库访问守卫：${violations.length} 处违规`);
		for (const v of violations) {
			console.error(`   ${SRC}/${v.file}:${v.line}  [${v.rule}]`);
			console.error(`       ${v.message}`);
		}
		console.error("");
		console.error("   正确写法（source.html / trace.html / index.html 同款）：");
		console.error('     解析期：A.expectGraphics();');
		console.error('     画图处：const G = await A.graphLib(t("...nolib"));');
		process.exit(1);
	}
	console.log(`✅ 图库访问守卫：${files.length} 个页面模板都只走 Anotify.graphLib()`);
}

async function selfTest() {
	const cases = [
		{
			name: "green: graphLib + expectGraphics",
			src: '<script>\nA.expectGraphics();\nconst G = await A.graphLib(t("x"));\n</script>',
			want: [],
		},
		{
			name: "green: 注释里提到 AnotifyGraphs 不算违规",
			src: "<!-- 别直接读 window.AnotifyGraphs，走 graphLib -->\n<p>hi</p>",
			want: [],
		},
		{
			name: "red: 裸读图库全局",
			src: '<script>\nif (!window.AnotifyGraphs) showError();\n</script>',
			want: ["no-direct-graph-lib"],
		},
		{
			name: "red: 用了 graphLib 但没挂门闩",
			src: '<script>\nconst G = await A.graphLib(t("x"));\n</script>',
			want: ["missing-expect-graphics"],
		},
		{
			name: "red: 两个都犯（行号各自指向真实位置）",
			src: '<script>\nA.mountLayout({});\nconst G = await A.graphLib(t("x"));\nwindow.AnotifyGraphs.taskChain();\n</script>',
			want: ["no-direct-graph-lib", "missing-expect-graphics"],
		},
	];
	let failed = 0;
	for (const c of cases) {
		const got = scanTemplate("fixture.html", c.src).map((v) => v.rule).sort();
		const want = [...c.want].sort();
		const ok = JSON.stringify(got) === JSON.stringify(want);
		console.log(`${ok ? "✅" : "❌"} ${c.name}${ok ? "" : ` —— want ${JSON.stringify(want)}, got ${JSON.stringify(got)}`}`);
		if (!ok) failed += 1;
	}
	// 行号断言：违规必须指向真实行，不能是一句「某处」。
	const lineCheck = scanTemplate("fixture.html", '<script>\n\nif (window.AnotifyGraphs) x();\n</script>');
	const lineOk = lineCheck[0] && lineCheck[0].line === 3;
	console.log(`${lineOk ? "✅" : "❌"} line numbers point at the offending line`);
	if (!lineOk) failed += 1;
	return failed === 0;
}

const isDirectRun =
	process.argv[1] &&
	path.resolve(process.argv[1]) === path.resolve(fileURLToPath(import.meta.url));
if (isDirectRun) {
	if (process.argv.includes("--self-test")) {
		selfTest()
			.then((passed) => process.exit(passed ? 0 : 1))
			.catch((e) => {
				console.error(e);
				process.exit(1);
			});
	} else {
		main().catch((e) => {
			console.error(e);
			process.exit(1);
		});
	}
}
