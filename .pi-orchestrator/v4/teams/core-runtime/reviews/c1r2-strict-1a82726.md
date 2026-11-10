# C1-R2 final independent gate: **BLOCKED**

Reviewed only frozen commit `1a827268969c9874a6764c07e5a57e1cb917c8e3` through `git show`. No live-code reads, tests, builds, writes, or children.

## 1. Facts and blocking finding

**Major — internal identifier grouping remains accepted, contrary to the supplied whole-expression-only approval.**

- `internal/store/migrate.go:734` unwraps the **whole predicate**, as approved.
- However, `internal/store/migrate.go:738–742` additionally unwraps the **identifier operand**. Line 739 explicitly preserves `(emit_id)`.
- Static counterexample: `WHERE (emit_id) IS NOT NULL` reaches:
  - `operator = 3`;
  - identifier tokens `["(", "EMIT_ID", ")"]`;
  - operand `unwrap` produces `["EMIT_ID"]`;
  - the final comparison returns **true**.
- `internal/store/v4test/format_test.go:841` retains a positive fixture:
  `WHERE /* only named emits */ (([emit_id]) IS NOT NULL)`.
  After removing the outer expression wrapper, the remaining `([emit_id])` is **internal operand grouping**, not whole-expression wrapping.

This is a confirmed **strict acceptance-contract violation**, not evidence of a remaining non-NULL uniqueness bypass. The stated approval and this retained positive expectation cannot both be certified.

**Minimum closure requirement:** the approved predicate grammar and the operand-grouped positive expectation must be reconciled by the parent. Under the supplied approval, internal operand grouping cannot receive acceptance. Escalate this contradiction; do not initiate a third retry.

## 2. Predicate flow and checks

Frozen flow:

`DDL string → tokenization/comment removal → identifier quote tagging → first unquoted WHERE → optional terminal semicolon removal → whole-expression unwrap → operand unwrap → suffix comparison`

Evidence: `internal/store/migrate.go:682–742`.

The previously harmful paths are now rejected by inspection:

- `emit_id IS (NOT NULL)` leaves grouped RHS tokens; the required final `IS`, `NOT`, `NULL` sequence fails.
- Whole-expression wrapping does not erase that RHS grouping.
- `emit_id IS NOT (NULL)` likewise fails.
- Extra operators/conjuncts, `ISNULL`, grouped keywords, and quoted operator keywords cannot satisfy the final comparison.

Evidence: `internal/store/migrate.go:712–742`; corresponding new rejection fixtures at `internal/store/v4test/format_test.go:811–813`.

Comment/quote handling remains intact:

- Complete line/block comments are discarded.
- Double-quoted, backtick, and bracket identifiers receive a sentinel, preventing quoted `WHERE` or operator keywords from masquerading as syntax.
- Single-quoted strings remain distinct from identifiers.

Evidence: `internal/store/migrate.go:682–701`. No additional confirmed comment/quote bypass was found within this scoped inspection.

Legitimate baseline and whole-expression cases remain accepted by the code: `emit_id IS NOT NULL` and `("emit_id" is not null)` at `internal/store/v4test/format_test.go:841`.

## 3. Regression protections

The added test asserts two duplicate non-NULL identities for the inverted fixtures before requesting refusal: `internal/store/v4test/format_test.go:820–830`.

The refusal helper checks the snapshot **before** error classification, then checks `ErrSchemaInvalid` and the named `"emits"` defect: `internal/store/v4test/format_test.go:976–991`.

Existing snapshot protection is unchanged:
- marker: `format_test.go:692`;
- named DDL: `format_test.go:694–706`;
- table-labelled rows: `format_test.go:716–717`;
- typed values: `format_test.go:722–746`.

Accepted reopen snapshot comparison also remains unchanged: `format_test.go:849–858`.

## 4. Verification and unresolved point

Historical red→green and `22.169s` verification were treated as reported evidence, not independently rerun proof.

Read-only verification commands: frozen `git show` diff and scoped frozen-source excerpts → exit **0**. Files changed: **none**. Tests added/run by this gate: **none**.

**DONE C1-R2 — review complete; gate rejected pending parent resolution of the grammar/fixture contradiction.**

{"ok":false,"reason":"Major strict predicate-contract violation: migrate.go:740 still accepts internally grouped identifiers, and format_test.go:841 preserves that conflicting positive fixture. Escalate; no third retry."}
