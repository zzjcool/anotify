# Canonical worktree path guard SOP

Parent adjudication 2026-11-10: old A 'replica drift' was literal .pi/subagents vs .pi-subagents cwd typo. Exact tool receipts and module package names proved differing paths, not production/harness/Node failure. Wrong-path scratch is parent-owned; never clean/copy/move it. No available sop_save tool discovered; reusable ruling persisted here rather than claiming tool success.

1. Capture actual manager pwd/git --show-toplevel from tools/JSON. Freeze FULL canonical absolute path in short leaf card.
2. Every leaf bash starts `cd CANONICAL && test "$(pwd -P)" = CANONICAL && test "$(git rev-parse --show-toplevel)" = CANONICAL && ...`. No fallback parent module/no auto-mkdir on failure.
3. Relative read/edit/write tools only after actual leaf cwd confirmed; no invented host-root relative location.
4. First read hashes of shared fixture files must equal manager canonical checkpoint hashes before edits. Own subsequent known edits explain later changes, never infer replicas from unrelated path.
5. Compiler package-name containing .pi/subagents is WRONG-CWD negative evidence, not canonical compile gate. Verify canonical package github.com/zzjcool/anotify/internal/store with full original test compilation, not shortened suite.
6. Path/root/hash failure stops before write; report precise cwd/output. At most two meaningful retries; never continue repeated malformed-path probing or create a 'replacement' source tree.
7. High-context leaf may persist real edits/remaining errors and settle incomplete. Only after old writer settled, parent-approved fresh leaf same WT continues same scope/history. No new team, no acceptance fiction, no cleanup or budget reset.
