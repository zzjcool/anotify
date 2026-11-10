三项旧 major 中，非空 schema 判定与 baseline 字段/身份覆盖的修复未发现残留阻塞；UNIQUE 修复仍有一项代码级 major：谓词校验删除所有括号，会把 `emit_id IS (NOT NULL)` 误认成 `emit_id IS NOT NULL`，但前者只匹配 NULL 行，无法保护非 NULL emit 身份。报告会区分这一静态确认与未执行的运行时反例，也不会把已接受的测试结果当作修复证明。
# C1-R strict gate — **BLOCKED**

Reviewed fixed commit **`c573e4687d7a2c11dfc8d87fd80f217a34eb7c90`**, using `git show` for the five scoped files and reading `c1-gate-459069f.md` for the previous findings. No live source, tests/builds, writes, other worktrees, or children.

**Result:** Previous majors 1 and 3 are repaired by inspection. **Major 2 remains blocking**, through a predicate-normalization loophole.

## Facts: disposition of the three repairs

1. **Nonempty-schema classification — repaired by inspection.**
   The probe now collects all main-database user schema objects, not merely tables. Both queries exclude the **literal** `sqlite_` prefix using `substr`, retaining `sqliteXkeep`. Initialization rejects any collected user object before migration writes.
   Evidence: `internal/store/migrate.go:249-252`, `:275-277`, `:294-296`, `:334-339`.

2. **Relevant refusal fixtures are present.**
   Cases include an unrelated view, populated `sqliteXkeep`, an empty user table, a view/trigger, and a table/index. They invoke the unchanged-state refusal assertion. This is inspected coverage, not a newly executed PASS.
   Evidence: `internal/store/v4test/format_test.go:757-776`.

3. **UNIQUE key-shape checks — substantially repaired, but predicate acceptance remains unsound.**
   All `key=1` entries are retained, including expression entries; descriptors include column identity, ordinary/expression distinction, direction, and collation. Matching uses complete descriptor equality rather than a leading-column prefix. This closes the inspected trailing-key, expression-key, and collation-substitution routes.
   Evidence: `internal/store/migrate.go:527-536`, `:449`, `:650-665`.
   The remaining predicate defect is detailed below.

4. **Complete baseline field/identity coverage — repaired by inspection.**
   The reference layout is derived from the embedded baseline, including the ledger. Every reference table is checked; missing/extra columns, declared type/default/nullability/PK metadata, table flags, PK descriptors, and FK definitions are compared. Required UNIQUE identities come from that same reference.
   Evidence: `internal/store/migrate.go:407-461`, `:477-513`, `:521-544`, `:548-562`, `:580-603`.
   Consequently, the previously omitted `namespace_configs.vars`, `task_attempts.machine_limits`, `nodes.name` UNIQUE, and attempt PK now participate. Their baseline definitions are at `internal/store/schema.sql:153`, `:195`, `:441`, `:449`.

5. **Malformed marker-4 ledger errors now receive the named refusal.**
   Query/scan/record failures are wrapped with `ErrSchemaInvalid`; unmarked malformed ledgers use `ErrUnsupportedFormat`. Invalid storage types, duplicate versions, and nonpositive versions are rejected.
   Evidence: `internal/store/migrate.go:263-269`, `:799-815`.

## Confirmed blocker — **Major: parentheses can invert emit coverage**

**Counterexample input** for an otherwise valid marker-4 database:

```sql
DROP INDEX idx_emits_task_emit;
CREATE UNIQUE INDEX replacement_emit ON emits(task_id, emit_id)
WHERE emit_id IS (NOT NULL);
```

This SQL was **not executed during this review**. The defect is confirmed by fixed-code inspection and SQL expression semantics:

- `NOT NULL` evaluates to NULL. Thus `emit_id IS (NOT NULL)` means **`emit_id IS NULL`**, not `emit_id IS NOT NULL`. It excludes every non-NULL emit identity from protection.
- `validEmitPredicate` removes **every parenthesis**, checking only balance, then compares the flattened words with `EMIT_ID IS NOT NULL`. The counterexample therefore satisfies its acceptance condition.
  Evidence: `internal/store/migrate.go:707-729`.
- The index’s ordered key descriptors match the required `(task_id, emit_id)` descriptors. Its `partial=true` and erroneous `emitPredicate=true` satisfy the UNIQUE matching condition.
  Evidence: `internal/store/migrate.go:637-643`, `:445-450`.
- The required baseline protects **non-NULL** emit IDs, explicitly preserving emit idempotency. The replacement protects the opposite subset and permits duplicate non-NULL `(task_id, emit_id)` identities with distinct row IDs.
  Evidence: `internal/store/schema.sql:460-473`.

**Minimum necessary correction:** This non-baseline predicate must receive `ErrSchemaInvalid` without changing business values, DDL, or marker. Acceptance must distinguish semantic grouping from harmless parentheses around the legitimate predicate. The existing negative cases do not include this grouping counterexample (`internal/store/v4test/format_test.go:781-792`).

This is a residual instance of **previous major 2**, not an expanded G1 finding.

## Key call chain and write boundary

- `migrate(db *sql.DB, modify func(*migrationSet))` → `migrateWith(db, set migrationSet, hooks map[int]func(*sql.Tx) error)` (`internal/store/migrate.go:149-157`).
- Probe result shape: `{marker int, ledger bool, versions map[int]bool, tables []string, objects []string}` (`:222-232`).
- Marker 4 → layout validation; otherwise → genuinely-new-database requirement. Both precede migration application (`:173-193`).
- New baseline → `applyBaselineMigration` → `applyMigrationTx(..., newBaseline=true)`. Marker, ledger DDL, baseline DDL, and ledger insertion use the same transaction, with deferred rollback before commit (`:754-795`).

No transaction-splitting regression was found in this scoped inspection.

## Regression, simplicity, and honest pending coverage

- Preservation snapshots now compare schema DDL, marker, and complete typed row values—not just counts. A same-count overwrite detection case exists (`internal/store/v4test/format_test.go:686-754`, `:927-937`).
- Schema-derived reference metadata removes the former incomplete handwritten layout checklist, but predicate token flattening introduces the blocking semantic shortcut above.
- Original colliding-view cases are explicitly **pre-initialization refusals**, not mid-transaction rollback evidence. White-box DDL/ledger/marker failure injection remains honestly pending; this is **not treated as a claimed-tested failure** (`internal/store/v4test/format_test.go:492-526`; `internal/store/v4test/migrate_test.go:15-17`).

## Verification and delivery boundary

- Four mechanical checks and **194 external tests**: accepted as supplied, not independent proof of these repairs.
- First-red/green runtime logs: **not examined here**. Runtime execution of the new counterexample remains pending; no executed failure or PASS is claimed.
- Commands: frozen `git show`, `git ls-tree`, and scoped historical `git diff` → successful.
- Changed files: **none**. Added tests: **none**.
- Lint/typecheck/tests/builds: **not run**.
- **DONE C1-R — review delivered; gate blocked.** No broader G1 or full-T1 completion claim.

{"ok": false, "reason": "C1-R blocked at c573e4687d7a2c11dfc8d87fd80f217a34eb7c90: confirmed residual major 2; predicate flattening accepts emit_id IS (NOT NULL), leaving non-NULL emit identities unprotected"}
