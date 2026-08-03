# Anotify 单二进制镜像
# 构建：docker build -t anotify .
# 运行：docker run -p 8080:8080 -e ANOTIFY_VAPID_PUBLIC_KEY=... -e ANOTIFY_VAPID_PRIVATE_KEY=... anotify

# ---- 前端生成（web-src → web/*.html）----
# sitegen 是 Go 程序，用 build 阶段（含 Go + 全源码）生成 web/*.html，
# 供 fe 阶段指纹、embed 阶段内嵌。

# ---- 前端指纹（web/ → internal/server/dist，供 go:embed）----
FROM node:22-alpine AS fe
WORKDIR /app
COPY scripts/hash.mjs ./scripts/hash.mjs
COPY --from=build /app/web ./web
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
# 先运行 sitegen 生成 web/*.html（构建期 i18n/布局合成）
RUN go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en
# 用指纹产物覆盖（go:embed 在编译期读取 internal/server/dist）
COPY --from=fe /app/internal/server/dist ./internal/server/dist
RUN go build -trimpath -ldflags="-s -w" -o /anotify ./cmd/server

# ---- 运行 ----
FROM alpine:3.20
RUN adduser -D -u 10001 anotify
WORKDIR /home/anotify
COPY --from=build /anotify /usr/local/bin/anotify
# 单二进制内嵌前端；仅需挂载数据目录持久化 sqlite
ENV ANOTIFY_ADDR=:8080 \
    ANOTIFY_STATIC= \
    ANOTIFY_DB=/home/anotify/anotify.db
EXPOSE 8080
USER anotify
VOLUME ["/home/anotify"]
ENTRYPOINT ["anotify"]
