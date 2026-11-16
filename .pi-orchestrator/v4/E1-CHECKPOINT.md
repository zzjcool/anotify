# E1 owner checkpoint（worker 自持，最新在上）

## 2 · 完成（E1 done，未 push）
- actual HEAD: `b30e998`（父 G2R1 基线 `6ecb938` 之上 1 个本地 commit）
- dirty: clean；in-flight cmd: 无
- 改动文件（全部在 E1 授权清单内）：
  `internal/store/dispatch_failure.go`、`internal/store/emission.go`、
  `internal/store/emission_receipt_test.go`（新）、`internal/route/emit.go`、
  `internal/route/emit_test.go`、`internal/route/route.go`、`internal/route/plan_test.go`、
  `internal/protocol/emit.go`、`internal/protocol/emit_test.go`（新）、
  `g2-resume-evidence/E1-EMIT-RECEIPT.md` + `g2-resume-evidence/e1-logs/`
- RED（修复前，/tmp 与 e1-logs 均有）：
  `go test ./internal/route/ -run 'TestEmitMissing|TestEmitStrictPinnedRef|TestSplitRefStrict|TestPlanFromSourceBadPinned' -count=1`
  → 6 个新/升级用例 FAIL：missing 回执 TaskID=<nil>、@2junk 被 200 放行、
  splitRef("ghost@2@3")=2、source `ghost@3junk` version=3。
- GREEN 4 固定 gate（真实 exit 0，tee 在 e1-logs/）：
  ① gofmt -l store/route/protocol 空 + git diff --check clean
  ② go vet store/route/protocol → exit 0
  ③ go test store/route/protocol -count=1 → exit 0（store 5.7s / v4test 41.1s / route 0.64s / protocol 0.05s）
  ④ go test ws/lease/source/trigger/logging/artifact/protocol -count=1 → exit 0
- 额外：store 局部 -race（EmitAuthorized/EmitResult/DispatchFailure/自定义 result）exit 0；
  `cmd/anode/internal/emitgw` 单包 exit 0（Node 成功判据未改）。
- 已知剩余/不属 E1：本 WT `internal/server/mux.go` 引用不存在的 `internal/executor`
  （HEAD 6ecb938 已如此）→ 全仓 `go build ./...`/`make ci`/`make contract` 本来红；
  E1 未触碰 internal/server/cmd/ws/FE；G3/G4 未开始。
