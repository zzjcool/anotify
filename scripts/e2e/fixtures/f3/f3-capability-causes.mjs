import assert from 'node:assert/strict';
import { acceptDialog } from './f3-dialogs.mjs';

const WIZARD_NAME = 'cap-cause-wizard';
const EDIT_NAME = 'cap-cause-edit';
const COLL = '/v1/namespaces/default/capabilities';
const FIELD = 'spec.params.properties.project.pattern';
const BAD_PARAMS = { type: 'object', properties: { project: { type: 'string', pattern: '[' } } };

const seedBody = (name) => ({
	apiVersion: 'anotify/v1',
	kind: 'Capability',
	metadata: { name },
	spec: {
		params: { type: 'object', properties: { project: { type: 'string', pattern: '^[a-z]+$' } } },
		run: { entrypoint: 'run.sh', files: { 'run.sh': '#!/bin/sh\necho hi\n' } },
	},
});

const marks = (page, id) =>
	page.evaluate((fieldId) => {
		const input = document.getElementById(fieldId);
		const body = document.querySelector('#cap-modal .modal-body');
		const hint = document.querySelector('[data-cause-for="' + fieldId + '"]');
		return {
			aria: input ? input.getAttribute('aria-invalid') : 'missing',
			invalid: input ? input.classList.contains('is-invalid') : null,
			hint: hint
				? { text: (hint.textContent || '').trim(), visible: !hint.classList.contains('hidden') }
				: null,
			hints: body ? body.querySelectorAll('[data-cause-for]').length : -1,
			reports: body ? body.querySelectorAll('.cause-report').length : -1,
		};
	}, id);

const stepChip = (page, n) =>
	page.waitForFunction(
		(expected) => {
			const chip = document.querySelector('#cap-modal .cap-step-current');
			return !!chip && chip.getAttribute('data-step-chip') === String(expected);
		},
		n,
		{ timeout: 10000 },
	);

const bannerShown = (page) =>
	page.evaluate(() => {
		const banner = document.getElementById('cap-banner');
		return !!banner && !banner.classList.contains('hidden');
	});

export async function capabilitySaveCauses(d) {
	const { page, goto, check } = d;

	await goto('capabilities', 'namespace=default', 'zh-CN');
	await page.locator('#caps-new').click();
	await page.locator('#cap-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.fill('#cap-name', WIZARD_NAME);
	await page.click('#cap-submit');
	await stepChip(page, 2);
	await page.fill('#cap-params', JSON.stringify(BAD_PARAMS, null, 2));
	await page.locator('#cap-files .cap-row-edit input').first().fill('run.sh');
	await page
		.locator('#cap-files .cap-row-edit textarea')
		.first()
		.fill('#!/bin/sh\necho hi\n');
	await page.selectOption('#cap-entrypoint', 'run.sh');
	await page.click('#cap-submit');
	await stepChip(page, 3);

	const wizardRespP = page.waitForResponse(
		(r) => r.request().method() === 'POST' && r.url().endsWith(COLL),
		{ timeout: 20000 },
	);
	await page.click('#cap-submit');
	const wizardResp = await wizardRespP;
	assert.equal(wizardResp.status(), 400, '坏 param pattern 必须被真实服务端拒绝（400），实际 ' + wizardResp.status());
	const wizardBody = await wizardResp.json();
	const wizardCauses = (wizardBody && wizardBody.details && Array.isArray(wizardBody.details.causes)
		? wizardBody.details.causes
		: []
	).filter(Boolean);
	assert.equal(wizardBody.reason, 'param_pattern_invalid', 'reason 必须是 param_pattern_invalid，实际 ' + wizardBody.reason);
	assert.ok(
		wizardCauses.some((c) => c.field === FIELD),
		'服务端必须点名 ' + FIELD + '，实际 ' + JSON.stringify(wizardCauses.map((c) => c.field)),
	);
	await page.waitForFunction(() => document.querySelectorAll('#cap-banner .cause-report-item').length > 0, null, {
		timeout: 10000,
	});
	const wizardMarks = await marks(page, 'cap-params');
	assert.equal(wizardMarks.aria, 'true', '#cap-params 必须 aria-invalid=true（新建向导）');
	assert.equal(wizardMarks.invalid, true, '#cap-params 必须带红框（新建向导）');
	const wizardMessage = String(wizardCauses.find((c) => c.field === FIELD).message || '').trim();
	assert.ok(wizardMarks.hint && wizardMarks.hint.visible, '#cap-params 下方必须出现一行 message，实际 ' + JSON.stringify(wizardMarks.hint));
	assert.ok(wizardMarks.hint.text.includes(wizardMessage), '提示必须原样带服务端 message，实际 ' + wizardMarks.hint.text);
	const wizardRows = await page.locator('#cap-banner .cause-report-item').count();
	assert.equal(wizardRows, wizardCauses.length, '清单每条 cause 一行，实际 ' + wizardRows);
	check('CAP-CAUSES-01 wizard marks #cap-params from a real 400 details.causes[] (' + wizardRows + ' cause rows)');

	await page.locator('#cap-banner [data-cause-jump="cap-params"]').click();
	await stepChip(page, 2);
	const jumped = await page.evaluate(() => (document.activeElement && document.activeElement.id) || '');
	assert.equal(jumped, 'cap-params', '点原因行必须先切回第 2 步再聚焦那个控件，实际焦点 ' + jumped);
	check('CAP-CAUSES-02 clicking the reason row walks the wizard back to the step holding the control');

	await acceptDialog(page, () => page.click('#cap-cancel'));
	await page.locator('#cap-modal').waitFor({ state: 'hidden', timeout: 20000 });
	await page.click('#caps-new');
	await page.locator('#cap-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	const wizardClean = await marks(page, 'cap-params');
	assert.equal(wizardClean.aria, null, '重开向导后 #cap-params 不许还挂着 aria-invalid');
	assert.equal(wizardClean.invalid, false, '重开向导后 #cap-params 不许还是红框');
	assert.equal(wizardClean.hints, 0, '重开向导后不许残留上一轮的 cause 提示，实际 ' + wizardClean.hints);
	assert.equal(wizardClean.reports, 0, '重开向导后不许残留上一轮的原因清单，实际 ' + wizardClean.reports);
	assert.equal(await bannerShown(page), false, '重开向导后横幅必须是收起的');
	check('CAP-CAUSES-03 reopening the wizard drops the previous round of marks (aria-invalid / hint / report)');
	await page.click('#cap-cancel');
	await page.locator('#cap-modal').waitFor({ state: 'hidden', timeout: 20000 });
	assert.equal((await d.response(COLL + '/' + WIZARD_NAME)).status, 404, '被拒绝的新建不许落库');

	const seeded = await d.response(COLL, seedBody(EDIT_NAME), 'POST');
	assert.ok(
		seeded.status === 201 || seeded.status === 200,
		'前置：先播一份真实能力当编辑对象，实际 ' + seeded.status,
	);

	await goto('capability', 'namespace=default&name=' + EDIT_NAME, 'zh-CN');
	await page.locator('#cap-actions [data-act="edit"]').waitFor({ state: 'visible', timeout: 20000 });
	await page.click('#cap-actions [data-act="edit"]');
	await page.locator('#cap-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	await page.fill('#cap-params', JSON.stringify(BAD_PARAMS, null, 2));

	const editorRespP = page.waitForResponse(
		(r) => r.request().method() === 'POST' && r.url().endsWith(COLL),
		{ timeout: 20000 },
	);
	await page.click('#cap-submit');
	const editorResp = await editorRespP;
	assert.equal(editorResp.status(), 400, '详情编辑器保存坏 pattern 必须 400，实际 ' + editorResp.status());
	const editorBody = await editorResp.json();
	assert.equal(editorBody.reason, 'param_pattern_invalid', 'reason 必须是 param_pattern_invalid，实际 ' + editorBody.reason);
	await page.waitForFunction(() => document.querySelectorAll('#cap-banner .cause-report-item').length > 0, null, {
		timeout: 10000,
	});
	const editorMarks = await marks(page, 'cap-params');
	assert.equal(editorMarks.aria, 'true', '#cap-params 必须 aria-invalid=true（详情编辑器）');
	assert.ok(editorMarks.hint && editorMarks.hint.visible, '#cap-params 下方必须出现一行 message（详情编辑器）');
	check('CAP-CAUSES-04 the detail editor marks #cap-params from the same real 400 (one contract, two pages)');

	await acceptDialog(page, () => page.click('#cap-cancel'));
	await page.locator('#cap-modal').waitFor({ state: 'hidden', timeout: 20000 });
	await page.click('#cap-actions [data-act="edit"]');
	await page.locator('#cap-modal:not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
	const editorClean = await marks(page, 'cap-params');
	assert.equal(editorClean.aria, null, '重开编辑器后 #cap-params 不许还挂着 aria-invalid');
	assert.equal(editorClean.invalid, false, '重开编辑器后 #cap-params 不许还是红框');
	assert.equal(editorClean.hints, 0, '重开编辑器后不许残留上一轮的 cause 提示，实际 ' + editorClean.hints);
	assert.equal(editorClean.reports, 0, '重开编辑器后不许残留上一轮的原因清单，实际 ' + editorClean.reports);
	assert.equal(await bannerShown(page), false, '重开编辑器后横幅必须是收起的');
	check('CAP-CAUSES-05 reopening the detail editor drops the previous round of marks (aria-invalid / hint / report)');
	await page.click('#cap-cancel');
	await page.locator('#cap-modal').waitFor({ state: 'hidden', timeout: 20000 });
}
