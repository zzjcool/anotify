# Anotify · 构建 / 测试 / 运行
# Go 依赖直连（不使用镜像代理）
export GOPROXY    := direct
export GOSUMDB    := sum.golang.org
export GOTOOLCHAIN := auto

PORT ?= 8080

.PHONY: help build fe test run dev docker docker-run integration tunnel keys clean

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

fe: ## 前端指纹：web/ → internal/server/dist/（content-hash + 引用改写，供 embed）
	node scripts/hash.mjs web internal/server/dist

dist: fe ## 同 fe（生成 embed 产物）

build: fe ## 构建单二进制（内嵌指纹后的前端）
	go build -trimpath -ldflags="-s -w" -o anotify ./cmd/server

test: ## 运行全部单元测试
	go test ./... -count=1

run: build ## 本地运行（需先设置 ANOTIFY_VAPID_* 环境变量）
	./anotify

dev: ## 开发模式：直接用 web/ 作为静态目录（不指纹）
	ANOTIFY_STATIC=./web go run ./cmd/server

integration: ## 集成测试（需服务已在 PORT 运行）
	BASE=http://localhost:$(PORT) ./scripts/integration.sh

docker: ## 构建 Docker 镜像
	docker build -t anotify .

docker-run: ## 运行 Docker 容器（需传入 VAPID 环境变量）
	docker run --rm -p $(PORT):8080 \
	  -e ANOTIFY_VAPID_PUBLIC_KEY=$$ANOTIFY_VAPID_PUBLIC \
	  -e ANOTIFY_VAPID_PRIVATE_KEY=$$ANOTIFY_VAPID_PRIVATE \
	  -e ANOTIFY_RP_ID=$$ANOTIFY_RP_ID \
	  -e ANOTIFY_RP_ORIGIN=$$ANOTIFY_RP_ORIGIN \
	  anotify

tunnel: ## Cloudflare 临时域名隧道（暴露到公网，供 iOS 验证）
	cloudflared tunnel --url http://localhost:$(PORT)

keys: ## 生成 VAPID 密钥对
	go run ./scripts/genkeys.go

clean: ## 清理构建产物
	rm -rf dist anotify internal/server/dist
