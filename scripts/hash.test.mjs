import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { buildInto, expectedDistPaths, MANIFEST_NAME } from "./hash.mjs";

async function walk(dir, rel = "") {
	const out = [];
	for (const e of await readdir(dir, { withFileTypes: true })) {
		const childRel = rel ? `${rel}/${e.name}` : e.name;
		if (e.isDirectory()) out.push(...(await walk(path.join(dir, e.name), childRel)));
		else out.push(childRel);
	}
	return out.sort();
}

async function fixture() {
	const root = await mkdtemp(path.join(tmpdir(), "anotify-hash-"));
	const src = path.join(root, "web");
	const out = path.join(root, "dist");
	await mkdir(path.join(src, "assets"), { recursive: true });
	await writeFile(path.join(src, "index.html"), '<link rel="stylesheet" href="ui.css">\n');
	await writeFile(path.join(src, "ui.css"), ".a { color: red }\n");
	await writeFile(path.join(src, "mock.js"), "export const mock = 1;\n");
	await writeFile(path.join(src, "manifest.webmanifest"), '{"icons":[{"src":"assets/x.png"}]}\n');
	await writeFile(path.join(src, "assets", "x.png"), Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x00]));
	await mkdir(out, { recursive: true });
	await writeFile(path.join(out, "stale-page.html"), "<h1>no source</h1>\n");
	await writeFile(path.join(out, ".source-stamp"), '{"stamp":"keep-me"}\n');
	return { root, src, out };
}

test("prune 只删本轮没产出的文件：残留消失、其它阶段的产物保留", async () => {
	const { root, src, out } = await fixture();
	try {
		await buildInto(src, out);
		assert.equal(existsSync(path.join(out, "stale-page.html")), false, "无源残留必须被删掉");
		assert.equal(
			await readFile(path.join(out, ".source-stamp"), "utf8"),
			'{"stamp":"keep-me"}\n',
			".source-stamp 属于其它阶段，不许被 prune 删除",
		);
		assert.equal(existsSync(path.join(out, "mock.js")), false, "dev-only 文件不进产物");
	} finally {
		await rm(root, { recursive: true, force: true });
	}
});

test("写入集的产物与 expectedDistPaths 的期望集逐路径相等（两侧不许各演一套）", async () => {
	const { root, src, out } = await fixture();
	try {
		await buildInto(src, out);
		const srcRels = await walk(src);
		const manifest = JSON.parse(await readFile(path.join(out, MANIFEST_NAME), "utf8"));
		const { files: expected, unresolved } = expectedDistPaths({ srcRels, manifest });
		const actual = await walk(out);
		assert.deepEqual(unresolved, [], "每个该指纹的源都必须在 manifest 里有条目");
		assert.deepEqual(actual, expected, "dist 实际文件集必须等于期望集（多一个 = 无源残留，少一个 = 残缺）");
		assert.match(actual.find((r) => r.startsWith("ui.")) ?? "", /^ui\.[0-9a-f]{8}\.css$/);
		assert.match(actual.find((r) => r.startsWith("assets/x.")) ?? "", /^assets\/x\.[0-9a-f]{8}\.png$/);
	} finally {
		await rm(root, { recursive: true, force: true });
	}
});

test("删掉源页面后再跑：上一轮的同名产物消失（#649 的最小回归）", async () => {
	const { root, src, out } = await fixture();
	try {
		await buildInto(src, out);
		assert.equal(existsSync(path.join(out, "index.html")), true);
		await rm(path.join(src, "index.html"));
		await buildInto(src, out);
		assert.equal(existsSync(path.join(out, "index.html")), false, "源已删的页面不许留在 dist 里");
	} finally {
		await rm(root, { recursive: true, force: true });
	}
});

test("源与目标是同一个目录 ⇒ 直接报错（否则 prune 会删掉源文件）", async () => {
	const root = await mkdtemp(path.join(tmpdir(), "anotify-hash-same-"));
	try {
		await writeFile(path.join(root, "ui.css"), ".a{}\n");
		await assert.rejects(() => buildInto(root, root), /同一个目录/);
	} finally {
		await rm(root, { recursive: true, force: true });
	}
});
