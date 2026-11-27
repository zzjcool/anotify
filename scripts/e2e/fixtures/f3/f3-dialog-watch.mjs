import assert from 'node:assert/strict';

const DIALOG = '#anotify-dialog';

export async function watchDialog(page) {
	await page.evaluate(() => {
		window.__f3DialogShown = false;
		const mark = () => {
			const dlg = document.getElementById('anotify-dialog');
			if (dlg && !dlg.classList.contains('hidden')) window.__f3DialogShown = true;
		};
		if (!window.__f3DialogWatch) {
			window.__f3DialogWatch = new MutationObserver(mark);
			window.__f3DialogWatch.observe(document.body, {
				subtree: true,
				childList: true,
				attributes: true,
				attributeFilter: ['class'],
			});
		}
		mark();
	});
}

export async function dialogShownEver(page) {
	return page.evaluate(() => window.__f3DialogShown === true);
}

export async function noDialogShown(page, why) {
	assert.equal(await page.locator(DIALOG + ':not(.hidden)').count(), 0, why);
}
