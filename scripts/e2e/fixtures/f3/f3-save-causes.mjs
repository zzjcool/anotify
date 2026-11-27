import assert from 'node:assert/strict';
import { acceptDialog } from './f3-dialogs.mjs';

const NAME = 'save-causes-e2e';
const POST = '/v1/namespaces/default/sources';

const reportOf = (page) =>
	page.evaluate(() => {
		const box = document.querySelector('#form-result .cause-report');
		if (!box) return null;
		return {
			title: (box.querySelector('.cause-report-title')?.textContent || '').trim(),
			rows: [...box.querySelectorAll('.cause-report-item')].map((n) => (n.textContent || '').trim()),
			jumps: [...box.querySelectorAll('[data-cause-jump]')].map((n) => n.getAttribute('data-cause-jump')),
			raw: (box.querySelector('.cause-report-raw pre')?.textContent || '').trim(),
		};
	});

const lastToast = (page) =>
	page.evaluate(() => {
		const bars = document.querySelectorAll('[data-error-bar]');
		const bar = bars[bars.length - 1];
		if (!bar) return null;
		const message = bar.children[1];
		return { text: (message && message.textContent ? message.textContent : '').trim(), whole: (bar.textContent || '').trim() };
	});

const hintOf = (page, id) =>
	page.evaluate((fieldId) => {
		const own = document.getElementById(fieldId + '-error');
		const made = document.querySelector('[data-cause-for="' + fieldId + '"]');
		const node = own || made;
		if (!node) return null;
		return { text: (node.textContent || '').trim(), visible: !node.classList.contains('hidden') };
	}, id);

export async function sourceSaveCauses(d) {
	const { page, goto, check } = d;
	await goto('sources', 'namespace=default');
	await page.locator('#btn-new').click();
	await page.locator('#src-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.locator('#f-kind').selectOption('poll');
	await page.locator('#f-poll-wrap:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });

	await page.locator('#f-name').fill(NAME);
	await page.locator('#f-poll-url').fill('http://x');
	await page.locator('#f-expr').fill('bad');
	await page.locator('#f-poll-timeout').fill('-1');

	const respP = page.waitForResponse(
		(r) => r.request().method() === 'POST' && r.url().endsWith(POST),
		{ timeout: 20000 },
	);
	await page.locator('#modal-save').click();
	const resp = await respP;
	assert.equal(resp.status(), 400, '坏 config 必须被真实服务端拒绝（400），实际 ' + resp.status());
	await page.waitForFunction(
		() => document.querySelectorAll('#form-result .cause-report-item').length > 0,
		null,
		{ timeout: 10000 },
	);
	const body = await resp.json();
	const causes = (body && body.details && Array.isArray(body.details.causes) ? body.details.causes : []).filter(Boolean);
	const fields = causes.map((c) => String(c.field || ''));
	for (const want of ['spec.config.url', 'spec.config.schedule', 'spec.config.timeoutMs']) {
		assert.ok(fields.includes(want), '服务端必须点名 ' + want + '，实际 ' + JSON.stringify(fields));
	}
	check('SAVE-CAUSES-01 real 400 carries per-field details.causes[] (' + fields.join(', ') + ')');

	const byField = new Map();
	for (const cause of causes) byField.set(String(cause.field || ''), String(cause.message || '').trim());
	for (const [field, id] of [
		['spec.config.url', 'f-poll-url'],
		['spec.config.schedule', 'f-expr'],
		['spec.config.timeoutMs', 'f-poll-timeout'],
	]) {
		assert.equal(await page.locator('#' + id).getAttribute('aria-invalid'), 'true', id + ' 必须 aria-invalid=true（' + field + '）');
		const hint = await hintOf(page, id);
		assert.ok(hint && hint.visible, id + ' 下方必须出现一行 message，实际 ' + JSON.stringify(hint));
		assert.equal(hint.text, byField.get(field), id + ' 的 message 必须与 cause 原文一致');
	}
	check('SAVE-CAUSES-02 url / schedule / timeoutMs each get aria-invalid + the server message');

	const focused = await page.evaluate(() => (document.activeElement && document.activeElement.id) || '');
	assert.equal(focused, 'f-poll-url', '焦点必须落到第一个出错的控件，实际 ' + focused);

	const report = await reportOf(page);
	assert.ok(report, '#form-result 里必须有原因清单（不是一行协议串）');
	assert.equal(report.title, '保存失败：' + causes.length + ' 处需要修改', '横幅标题必须逐字是「保存失败：N 处需要修改」，实际 ' + report.title);
	const labels = await page.evaluate((ids) => {
		const out = {};
		for (const id of ids) {
			const label = document.querySelector('label[for="' + id + '"]');
			out[id] = label ? (label.textContent || '').trim() : '';
		}
		return out;
	}, ['f-poll-url', 'f-expr', 'f-poll-timeout']);
	const controlOf = {
		'spec.config.url': 'f-poll-url',
		'spec.config.schedule': 'f-expr',
		'spec.config.timeoutMs': 'f-poll-timeout',
	};
	const expectedRows = causes.map((cause) => labels[controlOf[cause.field]] + '：' + String(cause.message || '').trim());
	assert.deepEqual(
		report.rows,
		expectedRows,
		'每条 cause 一行，内容必须逐字是「字段人话名：服务端 message」（不许叠前缀、不许改字），实际 ' + JSON.stringify(report.rows),
	);
	assert.deepEqual(report.jumps, ['f-poll-url', 'f-expr', 'f-poll-timeout']);
	const toast = await lastToast(page);
	assert.equal(toast.text, report.title, '保存失败的 toast 必须与横幅同口径，实际 ' + JSON.stringify(toast));
	await page.locator('#form-result [data-cause-jump="f-poll-timeout"]').click();
	const jumped = await page.evaluate(() => (document.activeElement && document.activeElement.id) || '');
	assert.equal(jumped, 'f-poll-timeout', '点清单里的一行必须跳到那个控件，实际 ' + jumped);
	check('SAVE-CAUSES-03 banner + toast say 保存失败：N 处需要修改，rows are exactly 人话名：message and jump to their control');

	assert.ok(report.raw.includes('invalid_source_config'), '折叠里的机器原文要保留 reason 码，实际 ' + report.raw);
	check('SAVE-CAUSES-04 reason code stays in the raw-JSON fold, not in the human list');

	await acceptDialog(page, () => page.locator('#modal-cancel').click());
	await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
	const after = await d.response(POST + '/' + NAME);
	assert.equal(after.status, 404, '被拒绝的源不许落库，实际 ' + after.status);

	await page.locator('#btn-new').click();
	await page.locator('#src-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	const residue = await page.evaluate(() => {
		const body = document.getElementById('src-modal-body');
		return {
			fields: ['f-poll-url', 'f-expr', 'f-poll-timeout'].map((id) => {
				const input = document.getElementById(id);
				return { id, aria: input.getAttribute('aria-invalid'), invalid: input.classList.contains('is-invalid'), value: input.value };
			}),
			hints: [...body.querySelectorAll('[data-cause-for]')].map((n) => n.getAttribute('data-cause-for')),
			report: body.querySelectorAll('.cause-report').length,
			result: (document.getElementById('form-result').textContent || '').trim(),
		};
	});
	for (const row of residue.fields) {
		assert.equal(row.aria, null, '重开后 ' + row.id + ' 不许还挂着上一轮的 aria-invalid');
		assert.equal(row.invalid, false, '重开后 ' + row.id + ' 不许还是上一轮的红框');
	}
	assert.deepEqual(residue.hints, [], '重开后不许残留上一轮的 cause 提示，实际 ' + JSON.stringify(residue.hints));
	assert.equal(residue.report, 0, '重开后不许残留上一轮的原因清单');
	assert.equal(residue.result, '', '重开后 #form-result 必须是空的，实际 ' + residue.result);
	assert.equal(residue.fields[1].value, '0 3 * * *', '重开后 cron 必须回到默认值，实际 ' + residue.fields[1].value);
	assert.equal(residue.fields[2].value, '10000', '重开后 timeoutMs 必须回到默认值，实际 ' + residue.fields[2].value);
	check('SAVE-CAUSES-05 reopening the editor drops the previous round of marks (aria-invalid / hint / report)');
	await page.locator('#modal-cancel').click();
	await page.locator('#src-modal').waitFor({ state: 'hidden', timeout: 20000 });
}
