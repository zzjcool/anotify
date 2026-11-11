# G4a · 构建产物对账（#334）

> 目的：让 `make build` 的产物**可复核**——二进制 sha256、版本注入来源、五份 spec 的合并
> hash、前端指纹各一行，和 CI / 发布 / 镜像用同一份事实。
>
> 本文件**不是**构建产物本身（仓库不提交任何二进制），而是「怎么构建、怎么对账」+
> 一次验收的实测值。重新生成：`make manifest`。

## 1. 产物清单（v4 只有两个产品二进制）

| 产物 | 入口 | 构建命令 | 说明 |
| --- | --- | --- | --- |
| `./anotify` | `cmd/server` | `make build`（`-tags embeddist`） | 中枢；内嵌指纹后的前端 |
| `./anode` | `cmd/anode` | `make build` / `make anode` | 节点；`serve` / `install` / `emit` |
| `/usr/local/bin/anotify`（镜像） | `cmd/server` | `make docker` | 中枢镜像 |
| `/app/anode`（镜像） | `cmd/anode` | `make docker` | 中枢镜像自带的**节点升级源**（`GET /v1/node/binary`） |
| `/devseed`（镜像 build 阶段） | `cmd/devseed` | `make docker` | 开发播种；**不进** runtime 镜像 |
| `anotify-anode` 镜像 | `cmd/anode` | `make docker-anode` | 节点镜像（容器即一台节点） |

v3 的 `./anotify-agent`、`./agent`、`Dockerfile.agent`、`cmd/agent` 已随节点产品换代为
`anode` 退役（#334），Makefile 不再有任何目标引用它们。

## 2. 版本注入（两处变量不同源，但必须是同一个串）

| 二进制 | `-ldflags -X` 变量 | 值 |
| --- | --- | --- |
| `anotify` / `devseed` | `internal/version.Version` | `$(VERSION)`（缺省 `git describe --tags --always --dirty`） |
| | `internal/version.Commit` | `$(GIT_SHA)` |
| | `internal/version.BuiltAt` | `$(BUILT_AT)`（RFC3339 UTC） |
| `anode` | `main.version` | `$(VERSION_LABEL)` = `$(VERSION)+$(GIT_SHA)` |
| | `main.commit` | `$(GIT_SHA)`（空仓回落 `unknown`） |

为什么 anode 的 `main.version` 带 `+<commit>`：中枢用 `internal/version.String()`
（`Version+Commit`）当**升级目标版本**（`internal/server/version_seam.go`），节点在 hello 里
上报 `main.version`（`cmd/anode/internal/app`）。两处不等 →「已是最新」永远判不出来，
节点会被反复升级。`isUniqueVersion` 仍然把 `dev` / `*dirty*` 当作不可比较的脏构建。

核对：

```bash
./anotify --version   # anotify <version>+<commit> (<builtAt>)
./anode --version     # {"version":"<version>+<commit>","commit":"<commit>"}
```

Docker 构建用同一套公式：`Dockerfile` 的 build-args（`VERSION` / `GIT_SHA` / `BUILT_AT`）
分别喂给两条 ldflags；镜像内 `/app/anode --version` 与宿主 `./anode --version` 一致。

## 3. 一键对账

```bash
make manifest
# version:  <VERSION_LABEL>
# git:      <git short sha>
# built_at: <RFC3339 UTC>
# <sha256>  anotify
# <sha256>  anode
# spec:     <sha256 of `cat api/*.yaml`>
# fe:       <sha256 of internal/server/dist/manifest.json>
```

- `spec` 是 `api/*.yaml` 按 shell glob 顺序拼接后的 sha256：五份 spec 是 exact-five，
  改任意一份都会变。
- `fe` 是 `scripts/hash.mjs` 产出的哈希清单文件本身；它变了 = 前端资源集合/内容变了，
  embed 进 `anotify` 的字节也随之变。

## 4. 本次验收实测

构建时 head = `530c578`（干净工作区；不是 `-dirty`），`make manifest` 原样输出：

```
version:  v0.2.0-266-g530c578+530c578
git:      530c578
built_at: 2026-11-11T01:57:31Z
706459f300cfe7779eb52e07bb48bb55819fdf14e25a4adea24eb3669c9964ba  anotify
ce6bc30a86b6c6b004c21ea7afe1cb90f8bf4de7987b6a92b52c0263556d3ba7  anode
spec:     4f52f2ad1e959de8c48f332e6e9e01d2e8b6a9aae159db78ae9f75ebacccaccd  (api/*.yaml)
fe:       f05d398db558251dd4dad41f0dff280961d99cef12ce541e56dd6981b4129910  (internal/server/dist/manifest.json)
```

两个二进制自报同一份版本串：

```
$ ./anotify --version
anotify v0.2.0-266-g530c578+530c578 (2026-11-11T01:57:31Z)
$ ./anode --version
{"version":"v0.2.0-266-g530c578+530c578","commit":"530c578"}
```

> 注：`530c578` 是**构建时**的 head。之后再提交（即使只改文档）重跑 `make manifest`，
> `version` / `git` 两行会变成新的 head，两个二进制的 sha256 也会变（`anotify` 进
> `internal/version.Commit`、`anode` 进 `main.version`/`main.commit`，两者都把 sha 编进
> 了二进制）；`spec` / `fe` 两行与源码无关，应保持不变。

镜像侧同一份事实（`make docker` 之后，镜内二进制与构建时 `./anode` 逐字相同）：

```
$ docker run --rm --entrypoint /app/anode anotify --version
{"version":"<VERSION_LABEL>","commit":"<git sha>"}   # 实测与同一棵树的 ./anode --version 一致
```

镜像的 build-args（`VERSION` / `GIT_SHA` / `BUILT_AT`）由 `make docker` 从 Makefile 同一组
变量传入；直接在容器里 `docker build` 不传 args 时回落 `VERSION=dev`（与裸 `go build` 一致）。

### 4.1 验收命令（v4 工具链）

| 命令 | 结果 |
| --- | --- |
| `make build` | 0（产出 `./anotify` + `./anode`） |
| `make manifest` | 0（上表） |
| `make contract` | 0（含 `go build -o /dev/null ./cmd/anode`） |
| `make fe` / `make sitegen` | 0 |
| `make check-anode-skill` | 0（手册 ↔ 真二进制 ↔ install-anode.sh） |
| `go vet ./...` / `go build ./...` | 0 |
| `go test ./cmd/... -count=1` | 0 |
| `make test`（`go test ./... -count=1`） | 0 |
| `make docker` / `make docker-anode` | 0（镜像 smoke：`/health` 200，数据卷重启仍在） |
| `make ci` | **非 0**（两个**既有**阻塞，基线同样红；`internal/**` 在本 issue 只读，交 owner）：① `bench-dispatch` → `internal/store/bench_dispatch_test.go` 仍是 v3 形状（`route.Dispatcher` / `Task.InstanceID` / `ReleaseInstance`…，与 `bench_test.go` 重名）；② root 运行下 `tmp-leak-guard` 的私有 TMPDIR（0700）让降权到 `nobody` 的 `cmd/anode` 权限用例失败，且长 socket 路径超 108 字节（非 root 运行不复现）。其余阶段（contract / gofmt / fe / vet / build / check-anode-skill / tmp-leak-guard 自检 / test / harness 自测）均绿 |

## 5. 已知边界

- **不提交二进制**：`anotify` / `anode` 已进 `.gitignore`；本文件只记录可复核的元数据。
- **`VERSION` 的 `-dirty` 后缀**：工作区有未提交改动时 `git describe` 会带 `-dirty`，
  这是**有意**的——脏构建不能被升级端点误判为「已最新」（`internal/server` 的
  `isUniqueVersion` 只信干净 tag）。发布构建来自干净 tag，标签形如 `v1.2.3+<sha>`。
- **交叉/多架构**：`make docker` 与 `push` 支持 `buildx` 多平台；各平台镜像里的
  `anode` 由同一份源码+同一组 build-args 构建，sha256 每平台不同（对应各自的二进制）。
