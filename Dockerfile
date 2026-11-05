# Anotify 单二进制镜像
# 构建：docker build -t anotify .
# 运行：docker run -p 8080:8080 -e ANOTIFY_VAPID_PUBLIC_KEY=... -e ANOTIFY_VAPID_PRIVATE_KEY=... anotify

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
# api/openapi.yaml 是 API 文档页「全部端点」的唯一来源（sitegen 在构建期解析它），
# 漏拷会让 sitegen 直接报错退出——宁可在构建阶段红，也不要静默出一个空表格。
COPY api/ ./api/
RUN go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en

# ---- 前端指纹（web/ → internal/server/dist，供 go:embed）----
FROM node:22-alpine AS fe
WORKDIR /app
COPY scripts/hash.mjs ./scripts/hash.mjs
COPY --from=sitegen /app/web ./web
RUN node scripts/hash.mjs web internal/server/dist

# ---- Go 构建（单二进制，CGO 关闭，纯 Go sqlite）----
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
# 三个二进制用**同一份**版本注入：/agent 是「中枢自带升级源」要发出去的那一份
# （CONTRACT-agent-upgrade §2），它的版本必须与中枢一致——否则节点升级上来还是
# dev，控制台永远显示「过期」，然后无限提示升级。参数与 Makefile 的
# VERSION_LDFLAGS 同源（VERSION 由 build-arg 传入，缺省 dev）。
ARG VERSION=dev
ARG GIT_SHA=
ARG BUILT_AT=
RUN VERSION_LDFLAGS="-X github.com/zzjcool/anotify/internal/version.Version=${VERSION} \
      -X github.com/zzjcool/anotify/internal/version.Commit=${GIT_SHA} \
      -X github.com/zzjcool/anotify/internal/version.BuiltAt=${BUILT_AT}" \
    && go build -trimpath -ldflags="-s -w $VERSION_LDFLAGS" -tags embeddist -o /anotify ./cmd/server \
    && go build -trimpath -ldflags="-s -w $VERSION_LDFLAGS" -o /agent ./cmd/agent \
    && go build -trimpath -ldflags="-s -w $VERSION_LDFLAGS" -o /devseed ./cmd/devseed

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
# 中枢同时是节点的**升级源**（CONTRACT-agent-upgrade §2）：把 agent 二进制带进镜像，
# 节点用 GET /v1/node/binary 下载它（node token 鉴权）。dev 与生产是**同一份**产物：
# 构建阶段编译一次，运行阶段只做拷贝。
#
# 放 /app/agent 是契约里的默认路径（ANOTIFY_AGENT_BIN 的缺省值），这里再显式写一次
# ENV，让「镜像里有什么」在 docker inspect 里也看得见。文件必须对运行用户可读
# （COPY 默认 0644，uid 10001 读得到）；不 chown 也不需要（只读，不替换自己）。
COPY --from=build /agent /app/agent
# 单二进制内嵌前端；仅需挂载数据目录持久化 sqlite
ENV ANOTIFY_ADDR=:8080 \
    ANOTIFY_STATIC= \
    ANOTIFY_DB=/data/anotify.db \
    ANOTIFY_AGENT_BIN=/app/agent
EXPOSE 8080
USER anotify
VOLUME ["/data"]
ENTRYPOINT ["anotify"]
# devseed 只在 build 阶段产出（不复制进 runtime）：本地开发用它播种。
# 生产镜像刻意只带 server（/usr/local/bin/anotify）与 agent（/app/agent）两个二进制。
# 见 docker-compose.dev.yml 的 seed/demo 服务。
