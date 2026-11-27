import assert from 'node:assert/strict';
import { dialogCancel, dialogOk, dialogText, waitDialog, waitDialogClosed } from './f3-dialogs.mjs';
import { dialogShownEver, noDialogShown, watchDialog } from './f3-dialog-watch.mjs';

const DIRTY = 'dirty-guard-e2e';
const CREATED = 'dirty-guard-created';
const CAP_MODAL = '#cap-modal';
const SRC_MODAL = '#src-modal';

const visible = (page, sel) => page.locator(sel + ':not(.hidden)').waitFor({ state: 'visible', timeout: 20000 });
const closed = (page, sel) => page.locator(sel).waitFor({ state: 'hidden', timeout: 20000 });

const stepChip = (page, n) =>
	page.waitForFunction(
		(want) => {
			const chip = document.querySelector('#cap-modal .cap-step-current');
			return !!chip && chip.getAttribute('data-step-chip') === String(want);
		},
		n,
		{ timeout: 20000 },
	);

const unsavedPrompt = async (page, entry) => {
	const text = await dialogText(page);
	assert.ok(text.includes('未保存'), entry + '：有未保存修改时必须先弹确认框（实际文案：' + text + '）');
	return text;
};

const asksOnReload = async (page, why) => {
	await page.keyboard.press('Tab'); // 真 Chrome 的 beforeunload 要本轮有用户手势，page.fill 不算
	await page.evaluate(() => {
		window.__f3DirtyGuardAlive = 'yes';
	});
	const prompted = page.waitForEvent('dialog', { timeout: 8000 });
	page.reload({ timeout: 8000 }).catch(() => 'aborted'); // 不 await：dismiss 之后它要等到超时才结算
	const dialog = await prompted;
	assert.equal(dialog.type(), 'beforeunload', why + '：刷新前必须先问，实际弹的是 ' + dialog.type());
	await dialog.dismiss();
	assert.equal(await page.evaluate(() => window.__f3DirtyGuardAlive), 'yes', why + '：在原生确认里选「留下」后不许真的刷新');
};

const reloadsSilently = async (page, why) => {
	let seen = null;
	let dismissError = null;
	const onDialog = (dialog) => {
		seen = dialog.type();
		dialog.dismiss().catch((error) => {
			dismissError = error;
		});
	};
	page.on('dialog', onDialog);
	try {
		await page.reload({ timeout: 20000 });
	} finally {
		page.off('dialog', onDialog);
	}
	assert.equal(dismissError, null, why + '：原生确认框没能关掉（' + dismissError + '）');
	assert.equal(seen, null, why + '：没有未保存修改时刷新不许弹确认，实际弹了 ' + seen);
};

const openCapEditor = async (page, name) => {
	await page.locator('#cap-actions [data-act="edit"]').waitFor({ state: 'visible', timeout: 20000 });
	await page.locator('#cap-actions [data-act="edit"]').click();
	await visible(page, CAP_MODAL);
	await page.waitForFunction((n) => document.getElementById('cap-form-name')?.value === n, name, { timeout: 20000 });
};

export async function editorDirtyGuard(d) {
	const { page, goto, check } = d;
	const cap = d.fx('capability');

	await goto('capability', 'namespace=default&name=' + cap, 'zh-CN');
	await openCapEditor(page, cap);
	await page.fill('#cap-display-name', DIRTY);
	await page.keyboard.press('Escape');
	await waitDialog(page);
	await unsavedPrompt(page, '能力编辑器 Esc');
	await dialogCancel(page).click();
	await waitDialogClosed(page);
	assert.equal(await page.locator(CAP_MODAL + ':not(.hidden)').count(), 1, '点「继续编辑」后能力编辑器必须还开着');
	assert.equal(await page.inputValue('#cap-display-name'), DIRTY, '点「继续编辑」后表单里的输入必须保留');
	await page.keyboard.press('Escape');
	await waitDialog(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, CAP_MODAL);
	check('DIRTY-GUARD-01 capability editor Esc asks before discarding; 继续编辑 keeps the modal open with the typed value, 放弃修改 closes it');

	for (const entry of [
		['✕', () => page.locator('#cap-modal-close').click()],
		['取消', () => page.locator('#cap-cancel').click()],
		['遮罩', () => page.locator(CAP_MODAL).click({ position: { x: 8, y: 8 } })],
	]) {
		await openCapEditor(page, cap);
		await page.fill('#cap-display-name', DIRTY + '-' + entry[0]);
		await entry[1]();
		await waitDialog(page);
		await unsavedPrompt(page, '能力编辑器 ' + entry[0]);
		await dialogOk(page).click();
		await waitDialogClosed(page);
		await closed(page, CAP_MODAL);
	}
	check('DIRTY-GUARD-02 ✕ / 取消 / 点遮罩 三个入口与 Esc 同一条闸门：有改动就先确认，确认后才真的关');

	await openCapEditor(page, cap);
	await page.fill('#cap-display-name', DIRTY + '-reload');
	await asksOnReload(page, '能力编辑器');
	assert.equal(await page.inputValue('#cap-display-name'), DIRTY + '-reload', '刷新被拦下后能力编辑器里的输入必须还在');
	await page.locator('#cap-cancel').click();
	await waitDialog(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, CAP_MODAL);
	await reloadsSilently(page, '能力编辑器（放弃修改后）');
	check('DIRTY-GUARD-03 capability editor: 刷新/关标签走原生 beforeunload，选「留下」后草稿还在；放弃修改后刷新不再问');

	await goto('capabilities', 'namespace=default', 'zh-CN');
	await page.locator('#caps-new').click();
	await visible(page, CAP_MODAL);
	await page.fill('#cap-name', DIRTY);
	await page.locator('#cap-cancel').click();
	await waitDialog(page);
	await unsavedPrompt(page, '新建向导 取消');
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, CAP_MODAL);
	check('DIRTY-GUARD-04 the new-capability wizard asks before discarding a typed name and closes only after 放弃修改');

	for (const entry of [
		['Esc', () => page.keyboard.press('Escape')],
		['✕', () => page.locator('#cap-modal-close').click()],
		['遮罩', () => page.locator(CAP_MODAL).click({ position: { x: 8, y: 8 } })],
	]) {
		await page.locator('#caps-new').click();
		await visible(page, CAP_MODAL);
		await page.fill('#cap-name', DIRTY + '-wiz-' + entry[0]);
		await entry[1]();
		await waitDialog(page);
		await unsavedPrompt(page, '新建向导 ' + entry[0]);
		await dialogOk(page).click();
		await waitDialogClosed(page);
		await closed(page, CAP_MODAL);
	}
	check('DIRTY-GUARD-05 wizard Esc / ✕ / 点遮罩 三个入口都先确认，确认后才真的关');

	await page.locator('#caps-new').click();
	await visible(page, CAP_MODAL);
	await page.fill('#cap-name', DIRTY + '-wiz-reload');
	await asksOnReload(page, '新建向导');
	assert.equal(await page.inputValue('#cap-name'), DIRTY + '-wiz-reload', '刷新被拦下后向导里已经填的名字必须还在');
	await page.locator('#cap-cancel').click();
	await waitDialog(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, CAP_MODAL);
	check('DIRTY-GUARD-06 wizard: 刷新/关标签走原生 beforeunload，选「留下」后已填内容还在，确认后才关');

	await page.locator('#caps-new').click();
	await visible(page, CAP_MODAL);
	await watchDialog(page);
	await page.locator('#cap-cancel').click();
	await closed(page, CAP_MODAL);
	await noDialogShown(page, '没改任何字段就取消向导时不许弹确认框');
	assert.equal(await dialogShownEver(page), false, '干净关闭全程不许出现确认框');
	check('DIRTY-GUARD-07 a clean wizard close adds no friction: no confirm layer at all, modal closes straight away');

	await page.locator('#caps-new').click();
	await visible(page, CAP_MODAL);
	await watchDialog(page);
	await page.fill('#cap-name', CREATED);
	await page.locator('#cap-submit').click();
	await stepChip(page, 2);
	await page.fill('#cap-params', JSON.stringify({ type: 'object' }, null, 2));
	await page.locator('#cap-files .cap-row-edit input').first().fill('run.sh');
	await page.locator('#cap-files .cap-row-edit textarea').first().fill('#!/bin/sh\necho dirty-guard\n');
	await page.selectOption('#cap-entrypoint', 'run.sh');
	await page.locator('#cap-submit').click();
	await stepChip(page, 3);
	await page.locator('#cap-submit').click();
	await page.waitForFunction(
		() =>
			(window.__f3Calls || []).some(
				(c) => c.method === 'POST' && c.url === '/v1/namespaces/default/capabilities' && c.httpStatus === 201 && c.settled === true,
			),
		null,
		{ timeout: 20000 },
	);
	const posts = (await d.calls()).filter((c) => c.method === 'POST' && c.url === '/v1/namespaces/default/capabilities');
	assert.equal(posts.length, 1, '向导创建只许发一次 POST …/capabilities，实际 ' + posts.length);
	assert.equal(posts[0].httpStatus, 201, '向导创建必须 201，实际 ' + posts[0].httpStatus);
	await closed(page, CAP_MODAL);
	await noDialogShown(page, '保存成功后不许再弹确认框');
	assert.equal(await dialogShownEver(page), false, '保存成功的整条路径上确认框一次都不许出现');
	check('DIRTY-GUARD-08 a successful wizard create closes the modal with no discard prompt at all');

	await goto('sources', 'namespace=default', 'zh-CN');
	await page.locator('#btn-new').click();
	await visible(page, SRC_MODAL);
	await page.fill('#f-name', DIRTY);
	await page.locator('#modal-cancel').click();
	await waitDialog(page);
	await unsavedPrompt(page, '源编辑器 取消');
	await dialogCancel(page).click();
	await waitDialogClosed(page);
	assert.equal(await page.locator(SRC_MODAL + ':not(.hidden)').count(), 1, '点「继续编辑」后源编辑器必须还开着');
	assert.equal(await page.inputValue('#f-name'), DIRTY, '点「继续编辑」后源编辑器里的输入必须保留');
	await page.locator('#modal-cancel').click();
	await waitDialog(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, SRC_MODAL);
	check('DIRTY-GUARD-09 source editor: 取消 asks, 继续编辑 keeps the modal and the value, 放弃修改 closes it');

	for (const entry of [
		['Esc', () => page.keyboard.press('Escape')],
		['✕', () => page.locator('#modal-close').click()],
	]) {
		await page.locator('#btn-new').click();
		await visible(page, SRC_MODAL);
		await page.fill('#f-name', DIRTY + '-src-' + entry[0]);
		await entry[1]();
		await waitDialog(page);
		await unsavedPrompt(page, '源编辑器 ' + entry[0]);
		await dialogOk(page).click();
		await waitDialogClosed(page);
		await closed(page, SRC_MODAL);
	}
	check('DIRTY-GUARD-10 source editor Esc / ✕ 与取消同一条闸门：有改动先确认，确认后才关');

	await page.locator('#btn-new').click();
	await visible(page, SRC_MODAL);
	await page.fill('#f-name', DIRTY + '-src-overlay');
	await page.locator(SRC_MODAL).click({ position: { x: 8, y: 8 } });
	assert.equal(await page.locator(SRC_MODAL + ':not(.hidden)').count(), 1, '源编辑器点遮罩不关（现状如此，不是丢稿入口），所以这里也不该弹确认');
	await noDialogShown(page, '源编辑器点遮罩本来就不关，不许因此弹确认框');
	assert.equal(await page.inputValue('#f-name'), DIRTY + '-src-overlay', '点遮罩后源编辑器的输入必须原样保留');
	check('DIRTY-GUARD-11 source editor 点遮罩不关闭、不弹确认（钉住实测现状：遮罩不是丢稿入口）');

	await asksOnReload(page, '源编辑器');
	assert.equal(await page.locator(SRC_MODAL + ':not(.hidden)').count(), 1, '刷新被拦下后源编辑器还开着');
	assert.equal(await page.inputValue('#f-name'), DIRTY + '-src-overlay', '刷新被拦下后源编辑器里的输入必须还在');
	await page.locator('#modal-cancel').click();
	await waitDialog(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	await closed(page, SRC_MODAL);
	await reloadsSilently(page, '源编辑器（放弃修改后）');
	check('DIRTY-GUARD-12 source editor: 刷新/关标签走原生 beforeunload，选「留下」后草稿还在；放弃修改后刷新不再问');

	await page.locator('#btn-new').click();
	await visible(page, SRC_MODAL);
	await watchDialog(page);
	await page.locator('#modal-cancel').click();
	await closed(page, SRC_MODAL);
	await noDialogShown(page, '没改任何字段就取消源编辑器时不许弹确认框');
	assert.equal(await dialogShownEver(page), false, '干净关闭全程不许出现确认框');
	check('DIRTY-GUARD-13 a clean source-editor close adds no friction: no confirm layer at all, modal closes straight away');

	await goto('capability', 'namespace=default&name=' + cap, 'zh-CN');
	await openCapEditor(page, cap);
	await watchDialog(page);
	await page.locator('#cap-cancel').click();
	await closed(page, CAP_MODAL);
	await noDialogShown(page, '没改任何字段就取消能力编辑器时不许弹确认框');
	assert.equal(await dialogShownEver(page), false, '干净关闭全程不许出现确认框');
	check('DIRTY-GUARD-14 a clean capability-editor close adds no friction: no confirm layer at all, modal closes straight away');
}
