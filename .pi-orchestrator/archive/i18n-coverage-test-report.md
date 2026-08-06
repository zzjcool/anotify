# i18n Coverage — Rendered-DOM Chinese-Residue Gate · Test Report

> Suite: `scripts/e2e/suites/i18n_coverage.mjs` ｜ Branch: feat/i18n-coverage ｜ Date: 2026-08-06
> Tester: anotify-tester ｜ Status: ✅ ALL GREEN

## Suite Purpose

Locks the i18n coverage work (batches 1–3) so that future regressions introducing
Chinese (CJK) characters into en/es pages are caught at the gate. Also verifies
ja pages use Japanese phrasing (not Chinese-specific wording).

## Coverage

| Section | Scope | Checks |
| --- | --- | --- |
| A | en/es × 7 pages (demo mode `?demo=1`) | no CJK residue in rendered DOM, no JS pageerror |
| A2 | en/es × message.html (demo, real notification ID) | no CJK residue |
| B | en/es × 4 key pages (real-data mode, injected session) | no CJK residue, no JS pageerror |
| C | ja × 5 pages (real-data mode) | no zh-only wording (总览/通知接收/已停用 etc.), contains JA markers (概要/通知/連携 etc.), no JS pageerror |
| D | en/keys disable confirm; ja/receivers device UI | interaction text in correct language |

## Result

- **Single-run**: 61 通过 / 0 失败
- **Related suites** (zero regression): i18n ✅, frontend ✅, lang_hint ✅
- **Full `make e2e`**: 13/13 suites green (12 existing + 1 new)

## Key Implementation Decision

Initial approach used `document.body.cloneNode(true).innerText` to strip exempt
DOM. **This was a bug**: `cloneNode` includes `<script>` elements, and a detached
clone's `innerText`/`textContent` does NOT exclude script content (unlike live
`document.body.innerText`). This caused 27K of JS source (containing `t("key",
"中文fallback")` literals) to leak into the scan, producing 26 false failures.

Fix: use live `document.body.innerText` (excludes `<script>` natively), then
strip exempt native-language names (中文 / 日本語) via string replace — these
are the only CJK strings that legitimately appear in all language versions
(language switcher shows native names by design).

## Product Bugs Found

None. All en/es pages render zero Chinese residue; all ja pages use correct
Japanese phrasing. The i18n coverage work (batches 1–3) is complete and verified.

## Residual Risks

- The CJK scan covers `innerText` (visible text). Hidden elements (display:none)
  are excluded by innerText, so a hidden Chinese string would not be caught.
  This is acceptable — hidden text is not user-visible.
- The ja "zh-only wording" check uses a fixed word list (总览/通知接收/已停用 etc.).
  New Chinese-only strings added in future would need to be added to the list.
  The JA_MARKERS check mitigates this by verifying the page IS Japanese.
