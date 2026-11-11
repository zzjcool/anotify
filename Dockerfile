# Anotify 中枢镜像（server + 内置节点升级源 anode）
# 构建：make docker（或 docker build -t anotify .）
# 运行：docker run -p 8080:8080 -e ANOTIFY_VAPID_PUBLIC_KEY=... -e ANOTIFY_VAPID_PRIVATE_KEY=... anotify
#
# v4 只包含两个产品二进制（#334）：
#   · /usr/local/bin/anotify  中枢（server，内嵌指纹前端）
#   · /app/anode              节点二进制 —— 升级源（GET /v1/node/binary，api/agent.yaml）
# v3 的 `/agent`（cmd/agent）已随节点产品换代为 anode 退役，镜像不再构建它。

# ---- 前端生成（web-src → web/*.html）----
# sitegen 是 Go 程序，在独立阶段生成 web/*.html（避免 fe ↔ build 循环依赖），
# 供 fe 阶段指纹、build 阶段 embed。
FROM golang:1.25-alpine AS sitegen
WORKDIR /app
ENV GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web-src/ ./web-src/
COPY web/ ./web/
# api/ 下的五份 v4 spec 是 API 文档页「全部端点」的唯一来源（sitegen 在构建期解析它），
# 漏拷会让 sitegen 直接报错退出——宁可在构建阶段红，也不要静默出一个空表格。
COPY api/ ./api/
RUN go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en

# ---- 前端指纹（web/ → internal/server/dist，供 go:embed）----
FROM node:22-alpine AS fe
WORKDIR /app
COPY scripts/hash.mjs ./scripts/hash.mjs
COPY --from=sitegen /app/web ./web
RUN node scripts/hash.mjs web internal/server/dist

# ---- Go 构建（server + anode + devseed，CGO 关闭，纯 Go sqlite）----
FROM golang:1.25-alpine AS build
WORKDIR /app
# 容器内用官方模块代理（proxy.golang.org，非第三方镜像）：
# GOPROXY=direct 会让 Go 用 git 克隆，而 alpine 无 git；代理 HTTPS 下载更可靠。
ENV GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用指纹产物覆盖（go:embed 在编译期读取 internal/server/dist；
# web/*.html 已由源码 COPY 进来，但 fe 阶段才是权威产物，故用 fe 覆盖）
COPY --from=fe /app/internal/server/dist ./internal/server/dist
# 版本注入（与 Makefile 的 VERSION_LDFLAGS / ANODE_LDFLAGS 同源）：
#   · server/devseed：internal/version.{Version,Commit,BuiltAt}
#   · anode：main.version / main.commit（**不同源**的包级变量）
# 中枢拿 internal/version.String()（= Version+Commit）当升级目标版本，节点在 hello 里
# 上报 main.version；两者必须**同一个串**，否则「已是最新」永远判不出来。
ARG VERSION=dev
ARG GIT_SHA=
ARG BUILT_AT=
RUN SERVER_LDFLAGS="-X github.com/zzjcool/anotify/internal/version.Version=${VERSION} \
      -X github.com/zzjcool/anotify/internal/version.Commit=${GIT_SHA} \
      -X github.com/zzjcool/anotify/internal/version.BuiltAt=${BUILT_AT}" \
    && ANODE_LABEL="${VERSION}" \
    && if [ -n "${GIT_SHA}" ]; then ANODE_LABEL="${VERSION}+${GIT_SHA}"; fi \
    && ANODE_LDFLAGS="-X main.version=${ANODE_LABEL} -X main.commit=${GIT_SHA:-unknown}" \
    && go build -trimpath -ldflags="-s -w $SERVER_LDFLAGS" -tags embeddist -o /anotify ./cmd/server \
    && go build -trimpath -ldflags="-s -w $ANODE_LDFLAGS" -o /anode ./cmd/anode \
    && go build -trimpath -ldflags="-s -w $SERVER_LDFLAGS" -o /devseed ./cmd/devseed

# ---- 运行 ----
FROM alpine:3.20
# tzdata：/v1/stats 的「今日」与热力图日桶按进程本地时区（TZ）计算。
# 不装 zoneinfo 时 alpine 的 time.Local 会退回 UTC，TZ 设了也不生效。
RUN apk add --no-cache tzdata
# 默认 UTC；部署时通过 TZ 环境变量切换（例如 TZ=Asia/Shanghai）。
ENV TZ=Etc/UTC
# 数据目录必须在切到非 root 用户**之前**建好并 chown：
# 否则 /data 归 root、权限 755，uid 10001 写不进自己的库——
# 容器能起来但一建 sqlite 就 permission denied（VOLUME 挂载时
# Docker 复制的是镜像里的属主，root:root）。
RUN adduser -D -u 10001 anotify \
    && mkdir -p /data \
    && chown anotify:anotify /data
WORKDIR /data
COPY --from=build /anotify /usr/local/bin/anotify
# 中枢同时是节点的**升级源**（api/agent.yaml：GET /v1/node/binary）：把节点二进制带进
# 镜像，节点用**自己的 node token** 下载它并原子替换。dev 与生产是**同一份**构建事实：
# 构建阶段编译一次，运行阶段只做拷贝。
#
# 环境变量名 `ANOTIFY_AGENT_BIN` 是 server 侧历史命名（internal/server/config.go），
# v4 它指向的文件就是 `anode`；显式设一次，让 `docker inspect` 里也能看出镜像带了什么。
COPY --from=build /anode /app/anode
# 单二进制内嵌前端；仅需挂载数据目录持久化 sqlite
ENV ANOTIFY_ADDR=:8080 \
    ANOTIFY_STATIC= \
    ANOTIFY_DB=/data/anotify.db \
    ANOTIFY_AGENT_BIN=/app/anode
EXPOSE 8080
USER anotify
VOLUME ["/data"]
ENTRYPOINT ["anotify"]
# devseed 只在 build 阶段产出（不复制进 runtime）：本地开发用它播种。
# 生产镜像刻意只带 server（/usr/local/bin/anotify）与节点升级源（/app/anode）。
# 见 docker-compose.dev.yml 的 seed/demo 服务与 docs/v4/anode-cli.md。
