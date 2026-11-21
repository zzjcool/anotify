import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { runsAsMain } from "./direct-run.mjs";

const DIR = mkdtempSync(path.join(tmpdir(), "anotify-direct-run-"));
process.on("exit", () => rmSync(DIR, { recursive: true, force: true }));

function seed(name, text = "export const x = 1;\n") {
	const p = path.join(DIR, name);
	mkdirSync(path.dirname(p), { recursive: true });
	writeFileSync(p, text);
	return p;
}

const urlOf = (p) => pathToFileURL(p).href;

test("真实路径 / 带 .. 的别名 / 符号链接：都判为直接调用（#507）", () => {
	const self = seed("self.mjs");
	assert.equal(runsAsMain(urlOf(self), self), true);
	assert.equal(runsAsMain(urlOf(self), path.join(DIR, "..", path.basename(DIR), "self.mjs")), true);
	const link = path.join(DIR, "alias.mjs");
	symlinkSync(self, link);
	assert.equal(runsAsMain(urlOf(self), link), true, "符号链接调用被判成「被 import」= 静默 exit 0");
});

test("argv[1] 是别人或缺失：不跑主逻辑（import 语义）", () => {
	const self = seed("self.mjs");
	assert.equal(runsAsMain(urlOf(self), seed("other.mjs")), false);
	assert.equal(runsAsMain(urlOf(self), ""), false);
});

test("拿不到 realpath（不存在/无权限）：选择执行，不 fail-open（#507）", () => {
	const self = seed("self.mjs");
	assert.equal(runsAsMain(urlOf(self), path.join(DIR, "gone.mjs")), true, "判不了就当自己是入口");
	assert.equal(runsAsMain(urlOf(path.join(DIR, "gone.mjs")), self), true, "自身路径判不了也当自己是入口");
});
