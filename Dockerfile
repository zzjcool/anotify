# Anotify 单二进制镜像
# 构建：docker build -t anotify .
# 运行：docker run -p 8080:8080 -e ANOTIFY_VAPID_PUBLIC=... -e ANOTIFY_VAPID_PRIVATE=... anotify

# ---- 前端指纹 ----
FROM node:22-alpine AS fe
WORKDIR /app
COPY scripts/hash.mjs ./scripts/hash.mjs
COPY web ./web
RUN node scripts/hash.mjs web dist

# ---- Go 构建 ----
FROM golang:1.25-alpine AS build
WORKDIR /app
ENV GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=fe /app/dist ./dist
RUN go build -trimpath -ldflags="-s -w" -o /anotify ./cmd/server

# ---- 运行 ----
FROM alpine:3.20
RUN adduser -D -u 10001 anotify
WORKDIR /home/anotify
COPY --from=build /anotify /usr/local/bin/anotify
COPY --from=build /app/dist ./dist
ENV ANOTIFY_ADDR=:8080 ANOTIFY_STATIC=./dist ANOTIFY_DB=./anotify.db
EXPOSE 8080
USER anotify
VOLUME ["/home/anotify"]
ENTRYPOINT ["anotify"]
