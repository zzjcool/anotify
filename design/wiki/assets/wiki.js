/* 移动端侧栏开合。全部使用 textContent / classList，不使用 innerHTML。 */
(function () {
	var btn = document.querySelector('#menuBtn');
	var side = document.querySelector('#sidebar');
	var scrim = document.querySelector('#scrim');
	if (!btn || !side || !scrim) return;

	function close() {
		side.classList.remove('open');
		scrim.classList.remove('show');
		btn.setAttribute('aria-expanded', 'false');
	}

	btn.addEventListener('click', function () {
		var open = side.classList.toggle('open');
		scrim.classList.toggle('show', open);
		btn.setAttribute('aria-expanded', open ? 'true' : 'false');
	});

	scrim.addEventListener('click', close);

	document.addEventListener('keydown', function (e) {
		if (e.key === 'Escape') close();
	});

	side.querySelectorAll('a').forEach(function (a) {
		a.addEventListener('click', close);
	});
})();
