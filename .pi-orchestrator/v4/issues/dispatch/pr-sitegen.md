## 目标

#333 删除 `api/openapi.yaml` 后，`internal/sitegen` 仍读单文件，导致 `make sitegen`/`make fe`/`make build` 在 #335 前不可用。本 PR 从已验收前端交付 `bf21f18` 移植 **exact-five 五文件读取器**，作为 #333 → #334 的桥接；不引入前端页面变更。

Base: `v4/integration`（#333 已合入）
Head: `fix/sitegen-five-reader` @ `0b3bb96f72555bfae1f4c1a6bd55247882766212`

## 改动（9 文件，全部在 sitegen 所有权内）

- `internal/sitegen/openapi.go`、`openapi_test.go`、`testdata/specs/{resource,events,agent,ingress,auth}.yaml`：与 `bf21f18` **逐字节一致**（`git hash-object` 对照）。
- `internal/sitegen/sitegen.go`：`LoadOpenAPI(path)` 改为五文件目录（委派 `LoadSpecDir`），无旧单文件 fallback；`api` tag 派生标签兜底。
- `cmd/sitegen/main.go`：`-openapi` 默认 `api/openapi.yaml` → `api`（exact-five 目录）；空值仍跳过；打印 `N operations from M/5 specs` 与具名诊断。

未改 `web/`、`api/`、`web-src/`、`Makefile`、`scripts/e2e/`、`internal/server/`。

## 验证（独立审核 + 实现者真实复跑）

| 命令 | EXIT |
|---|---|
| `gofmt -l internal/sitegen cmd/sitegen`（空） | 0 |
| `go vet ./internal/sitegen/... ./cmd/sitegen/...` | 0 |
| `go test ./internal/sitegen/... -count=1` | 0（49 tests） |
| `go test ./internal/... -count=1` | 0（22 包；server 70.4s / store+v4test 61.7s） |
| `go run ./cmd/sitegen -src web-src -out /tmp/... -langs zh-CN,en,ja,es` | 0（124 ops / 5/5 specs / 4 语；每语 124 行、21 组、0 裸 tag key） |
| `-openapi ""` | 0（真实空态） |
| 部分缺文件（2/5） | 0（58 ops + 3 条具名 warn） |
| 坏 `$ref` / 坏 pointer | 1（可判别：origin + ref + reason） |
| `python3 scripts/check_openapi.py` | 0（124 op，与 sitegen 对账一致） |

## 独立审核

`rev-sitegen`：**ok:true**（scope 正确、移植 byte-identical、行为独立复跑、测试无弱化）。3 minor 不阻塞（死 `LoadOpenAPI` 包装、死 Duplicates 循环、旧文案/`Dockerfile` 注释）。

## 交接给 #334 / #335

- **#334**：`scripts/e2e/suites/frontend.mjs:1521` 仍断言「端点清单不展示已弃用端点 `/v1/devices`」——那是按已删除的 v3 单文件 spec 写的；v4 `api/auth.yaml` 中 `/v1/devices` 是**现役非 deprecated** 端点。`make fe` 重新生成 `web/` 后该断言会红，需在 #334/#335 改成 v4 语义（或删除该负断言）。
- **#335**：7 个 tag 无 `web-src/locales` 词条（派生英文标签，非裸 key）；docs 文案仍写 `api/openapi.yaml`；`Dockerfile` 注释旧。均不阻塞 #334。

Closes 无（桥接 PR，关联 #333/#334）。
