# Anotify · 构建 / 测试 / 运行
# Go 依赖直连（不使用镜像代理）
export GOPROXY    := direct
export GOSUMDB    := sum.golang.org
export GOTOOLCHAIN := auto

PORT ?= 8080

# 构建版本号：git short sha + UTC 时间（+dirty 标记）。
# 空仓 / 非 git 目录下回退为空，sitegen 不传 -version。
# sitegen 目前只接受并打印该值，尚未写入 HTML footer。
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_DIRTY := $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_TIME := $(shell date -u +%Y-%m-%d_%H:%M:%S)
VERSION := $(if $(GIT_SHA),$(GIT_SHA)$(GIT_DIRTY) $(BUILD_TIME),)

.PHONY: help build fe sitegen test bench run dev docker docker-run up down push integration tunnel keys clean check-classes check-tailwind tailwind ci

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

sitegen: check-classes check-tailwind ## 构建期静态站点生成：web-src/（layouts+pages+locales）→ web/*.html + i18n js
	go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en,ja,es $(if $(VERSION),-version '$(VERSION)',)

check-classes: ## 前端死类守卫：校验 web-src 里每个 class 都落在设计系统 / Tailwind 工具类内（防自造未定义类）
	node scripts/check-classes.mjs

check-tailwind: ## 核对已提交的 web/tailwind.css 覆盖源码工具类（stdlib，不跑 Tailwind CLI）
	node scripts/check-tailwind-css.mjs

tailwind: ## 重新生成 web/tailwind.css（改了工具类后执行；需要 npm install）
	node scripts/build-tailwind.mjs

fe: check-classes sitegen ## 前端指纹：sitegen 生成 web/ 后 → internal/server/dist/（content-hash + 引用改写，供 embed）
	node scripts/hash.mjs web internal/server/dist

dist: fe ## 同 fe（生成 embed 产物）

# 默认 go build / go run 不嵌入 dist（开发期 ANOTIFY_STATIC 即可编译）。
# 生产单二进制加 -tags embeddist，且依赖本目标先跑的 fe。
build: fe ## 构建单二进制（内嵌指纹后的前端）
	go build -trimpath -ldflags="-s -w" -tags embeddist -o anotify ./cmd/server

test: ## 运行全部单元测试（不含 make e2e / make integration）
	go test ./... -count=1

# contract: 契约 §9 的静态断言（A2 / A4）。这几条不依赖运行期，放在 CI 里最快。
#
# A2（不变量 I1）：schema 里不得出现可执行的命令/脚本概念
# A4（不变量 I3）：kind 是推导量，**不得**落库
contract: ## CONTRACT.md §9 的静态不变量断言（A2/A4）+ openapi 与 mux 一致性
	@set -e; \
	if grep -riE 'argv|shell|script' internal/store/schema.sql >/dev/null; then \
		echo "❌ A2 失败：schema.sql 里出现 argv/shell/script（不变量 I1）"; \
		grep -riE 'argv|shell|script' internal/store/schema.sql; \
		exit 1; \
	fi
	@python3 scripts/check_contract.py
	@python3 scripts/check_openapi.py
	@echo "✅ 契约静态断言（A2/A4 + openapi 一致性）通过"

ci: contract ## PR 门禁：契约静态断言、gofmt、前端 embed 产物、vet、build、单元测试（不含 e2e / integration）
	@unformatted=$$(find . -name '*.go' ! -path './vendor/*' -exec gofmt -l {} +); \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "未通过 gofmt，请执行 gofmt -w：" "$$unformatted"; \
		exit 1; \
	fi
	$(MAKE) fe
	go vet ./...
	go build ./...
	go build -trimpath -ldflags="-s -w" -tags embeddist -o anotify ./cmd/server
	$(MAKE) test

bench: ## 运行全部基准测试（吞吐/分配热点；仅基准不跑单测）
	go test ./... -run=NONE -bench=. -benchmem -benchtime=1s

bench-report: ## 运行基准并输出到 bench.txt（留存对比基线）
	go test ./... -run=NONE -bench=. -benchmem -benchtime=1s > bench.txt 2>&1
	@echo "基准结果已写入 bench.txt"

run: build ## 本地运行（需先设置 ANOTIFY_VAPID_* 环境变量）
	./anotify

dev: check-classes sitegen ## 开发模式：起 server + cloudflared tunnel（读 .env.local，固定 dev.openaaas.org）
	./scripts/dev.sh

dev-local: check-classes sitegen ## 开发模式：只起 server，不起 tunnel（本地 localhost）
	NO_TUNNEL=1 ./scripts/dev.sh

integration: ## 集成测试（需服务已在 PORT 运行）
	BASE=http://localhost:$(PORT) ./scripts/integration.sh

e2e: ## 【固化门禁】全量端到端测试（并行模式，不含单测）
	./scripts/e2e/run_all.sh

e2e-parallel: ## 全量端到端测试（显式并行模式）
	./scripts/e2e/run_all.sh

e2e-serial: ## 全量端到端测试（串行模式，调试用）
	./scripts/e2e/run_all.sh --serial

e2e-one: ## 只跑某个 E2E 套件：make e2e-one S=auth_flow
	./scripts/e2e/run_all.sh $(S)

docker: ## 构建 Docker 镜像
	docker build -t anotify .

docker-run: ## 运行 Docker 容器（需传入 VAPID 环境变量）
	docker run --rm -p $(PORT):8080 \
	  -e ANOTIFY_VAPID_PUBLIC_KEY=$$ANOTIFY_VAPID_PUBLIC_KEY \
	  -e ANOTIFY_VAPID_PRIVATE_KEY=$$ANOTIFY_VAPID_PRIVATE_KEY \
	  -e ANOTIFY_RP_ID=$$ANOTIFY_RP_ID \
	  -e ANOTIFY_RP_ORIGIN=$$ANOTIFY_RP_ORIGIN \
	  anotify

up: ## compose 部署（需先 cp .env.compose.example .env 并填好）
	docker compose up -d

down: ## compose 停止并移除容器（数据卷保留）
	docker compose down

push: ## 构建并推送镜像到 GHCR（ghcr.io/zzjcool/anotify）
	docker buildx build --platform linux/amd64,linux/arm64 \
	  -t ghcr.io/zzjcool/anotify:latest \
	  --push .

tunnel: ## Cloudflare 命名隧道（固定域名 dev.openaaas.org）
	cloudflared tunnel run anotify

tunnel-url: ## 读取隧道公网地址（固定 dev.openaaas.org）
	@echo https://dev.openaaas.org

keys: ## 生成 VAPID 密钥对
	go run ./scripts/genkeys.go

clean: ## 清理构建产物
	rm -rf dist anotify internal/server/dist
