## Fresh C1 judgment — `1a827268969c9874a6764c07e5a57e1cb917c8e3`

**No confirmed Major/Critical bypass in the permitted evidence.** The grouped-operand repair is sound. `(([emit_id]) IS NOT NULL)` is accepted intentionally and is **not Major** under the clarified authority.

### Facts and predicate flow

1. **Quoted operators cannot masquerade as keywords.** Double-quoted, backtick and bracket tokens receive a `\x00` prefix before comparison; single-quoted literals remain quoted.
   Evidence: `internal/store/migrate.go:682–695`.

2. **Whole-expression parentheses are removed only when paired around the entire token sequence.** `unwrap` stops at the first return to depth zero and strips only if that position is the final token. It does not flatten internal grouping.
   Evidence: `internal/store/migrate.go:711–734`.

3. **The suffix must be exactly three unquoted, ungrouped tokens: `IS`, `NOT`, `NULL`.** Consequently, `IS (NOT NULL)`, `IS NOT (NULL)`, `IS(NOTNULL)` and quoted operator/RHS variants fail.
   Evidence: `internal/store/migrate.go:735–742`.

4. **The left operand must unwrap to one normalized `EMIT_ID` token.** Function calls, arithmetic, qualified references and ordinary other-column names retain additional/different tokens and fail. Exact identifier parentheses survive, including `(([emit_id]) IS NOT NULL)`.
   Evidence: `internal/store/migrate.go:738–742`.

**Internal flow:** DDL → tokenize/remove comments → mark quoted identifiers → locate `WHERE` → remove optional terminal semicolon → unwrap whole predicate → split final three operator tokens → unwrap identifier operand → compare.

### Fixture evidence

- Three negative fixtures cover plain/wrapped `IS (NOT NULL)` and `IS NOT (NULL)`. The first two insert duplicate non-NULL identities and assert two rows exist before requiring format refusal.
  Evidence: `internal/store/v4test/format_test.go:805–835`.

- Positive fixtures retain ordinary spelling, quoted whole-expression wrapping, paired `[emit_id]` operand wrapping, and an unconditional unique index. They require successful `store.Open` and unchanged snapshots.
  Evidence: `internal/store/v4test/format_test.go:840–858`.

These are **static assertions inspected**, not newly executed results.

### Risk / pending verification

The literal “only precise identifier” claim has one caveat: normalization uses Go’s Unicode-aware `strings.ToUpper` (`internal/store/migrate.go:695`). A quoted spelling such as `[emıt_id]` containing U+0131 can normalize to `EMIT_ID` and satisfy line 741. Thus the code guarantees the **normalized token**, not demonstrably SQLite-identical raw identifier spelling.

No harmful accepted SQLite/`store.Open` path for that caveat is established by the permitted evidence; I am **not classifying it Major/Critical** or claiming an executed bypass.

### Validation / changes

- Only fixed-commit `git show` inspection; commands exited **0**.
- Changed files: **none**.
- Added tests: **none**.
- Tests/builds/lint/typecheck: **not run**, as requested.
- No broader schema audit, other worktree inspection or child agents.

{"ok":true,"reason":"No confirmed Major/Critical bypass in scoped fixed-commit evidence; harmful operator grouping is rejected and authorized identifier wrapping is accepted. Unicode identifier normalization remains an unverified caveat."}
