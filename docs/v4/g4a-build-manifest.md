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

## 4. 本次验收实测（验收机上 `make manifest` 的输出）

```
# 见下方「实测」块（滚动更新：每次发版前重跑 make manifest 比对）
```

## 5. 已知边界

- **不提交二进制**：`anotify` / `anode` 已进 `.gitignore`；本文件只记录可复核的元数据。
- **`VERSION` 的 `-dirty` 后缀**：工作区有未提交改动时 `git describe` 会带 `-dirty`，
  这是**有意**的——脏构建不能被升级端点误判为「已最新」（`internal/server` 的
  `isUniqueVersion` 只信干净 tag）。发布构建来自干净 tag，标签形如 `v1.2.3+<sha>`。
- **交叉/多架构**：`make docker` 与 `push` 支持 `buildx` 多平台；各平台镜像里的
  `anode` 由同一份源码+同一组 build-args 构建，sha256 每平台不同（对应各自的二进制）。
