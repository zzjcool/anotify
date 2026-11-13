// #378：危险操作的统一弹层驱动。
//
// 迁移前：每个页面用原生 `window.confirm()`，fixture 靠 Playwright 的 `page.on('dialog')`
// 拦截并 accept/dismiss。迁移后：`Anotify.confirmDanger()` 会在文档里放一个
// `#anotify-dialog`（partials.js），确认/取消都是真实按钮 —— 所以 fixture 必须驱动
// 真实控件，不能再用 dialog 事件（否则 page.once('dialog') 永远不触发，测试会挂死到超时）。
//
// 这里保持旧断言的**语义**不变：
//   · dismiss ⇒ 零写入、对象保留；
//   · accept  ⇒ 恰好一次写入，且写入目标与旧断言一致；
//   · 弹层本身仍是"用户真正点得到的控件"，不是测试专用后门。
export const DIALOG = "#anotify-dialog";
const HIDE_MS = 5000;

/** 弹层里的确认按钮（危险动作用 .btn-danger）。 */
export const dialogOk = (page) => page.locator(`${DIALOG} [data-dialog-ok]`);
/** 弹层里的取消按钮（modal-foot 里的那个）。 */
export const dialogCancel = (page) => page.locator(`${DIALOG} [data-dialog-cancel]`);
/** 弹层头部关闭按钮。 */
export const dialogClose = (page) => page.locator(`${DIALOG} [data-dialog-close]`);

/** 等到弹层可见。 */
export async function waitDialog(page, timeout = HIDE_MS) {
	await dialogOk(page).waitFor({ state: "visible", timeout });
}

/** 等到弹层真的关掉（有 140ms 退场）。 */
export async function waitDialogClosed(page, timeout = HIDE_MS) {
	await page.locator(DIALOG).waitFor({ state: "hidden", timeout });
}

/** 弹层的完整可见文案（标题 + 正文 + 附注），用于断言本地化/后果说明。 */
export async function dialogText(page) {
	return (await page.locator(DIALOG).innerText()).replace(/\s+/g, " ").trim();
}

/**
 * 点触发控件 → 弹层出现 → 点确认。
 * @returns {Promise<string>} 弹层的可见文案（旧断言里 `dialog.message()` 的等价物）
 */
export async function acceptDialog(page, trigger) {
	await trigger();
	await waitDialog(page);
	const text = await dialogText(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	return text;
}

/**
 * 点触发控件 → 弹层出现 → 点取消（= 旧断言的 dialog.dismiss()）。
 * @returns {Promise<string>} 弹层的可见文案
 */
export async function dismissDialog(page, trigger) {
	await trigger();
	await waitDialog(page);
	const text = await dialogText(page);
	await dialogCancel(page).click();
	await waitDialogClosed(page);
	return text;
}

/**
 * 点触发控件 → 弹层出现 → 按 Esc 关掉（#378 的「弹层 Esc 可关」）。
 * @returns {Promise<string>} 弹层的可见文案
 */
export async function escapeDialog(page, trigger) {
	await trigger();
	await waitDialog(page);
	const text = await dialogText(page);
	await page.keyboard.press("Escape");
	await waitDialogClosed(page);
	return text;
}

/** 撤销条出现（删除成功后的「已删除 · 撤销（5s）」）。 */
export const undoBar = (page) => page.locator("#anotify-undo-host [data-undo-bar]");

/** 等撤销条可见。 */
export async function waitUndoBar(page, timeout = HIDE_MS) {
	await undoBar(page).last().waitFor({ state: "visible", timeout });
	return (await undoBar(page).last().innerText()).replace(/\s+/g, " ").trim();
}

/** 点撤销。 */
export async function clickUndo(page) {
	await undoBar(page).last().locator("[data-undo-action]").click();
}

/**
 * 弹层已经开着（触发不是一次普通 click，例如 selectOption / keyboard）时，直接点确认。
 * 用途：把 `page.on('dialog', d => d.accept())` 那种「全局自动确认」改写成「显式点真实按钮」。
 */
export async function acceptOpenDialog(page) {
	await waitDialog(page);
	const text = await dialogText(page);
	await dialogOk(page).click();
	await waitDialogClosed(page);
	return text;
}

/** 同上，但点取消。 */
export async function dismissOpenDialog(page) {
	await waitDialog(page);
	const text = await dialogText(page);
	await dialogCancel(page).click();
	await waitDialogClosed(page);
	return text;
}
