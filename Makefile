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
# shellcheck disable=SC1073,SC1050,SC1072,SC1009
# 下一行是 **Make 的** $(if ...) 函数，不是 shell 的 if。本文件是 Makefile，
# 但把 Makefile 映射成"shell 语言"的分析器会把它当 shell 解析并报语法错。
VERSION := $(if $(GIT_SHA),$(GIT_SHA)$(GIT_DIRTY) $(BUILD_TIME),)

.PHONY: help build fe demo-chain sitegen test bench run dev docker docker-run up down push integration tunnel keys dev-env dev-up dev-down dev-reset dev-logs dev-seed dev-demo dev-sh dev-tunnel-setup dev-tunnel-quick clean check-classes check-tailwind tailwind ci mock mock-local fe-preview fe-preview-port

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
	@unformatted=$$(git ls-files '*.go' | xargs -r gofmt -l 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "未通过 gofmt，请执行 gofmt -w：" "$$unformatted"; \
		exit 1; \
	fi
	$(MAKE) fe
	go vet ./...
	go build ./...
	go build -trimpath -ldflags="-s -w" -tags embeddist -o anotify ./cmd/server
	$(MAKE) test
	@# harness 自测（#215）：/health 的 boot 身份校验是"看不见的"修复——正常跑什么都不打印。
	@# 而这个缺陷的症状是「套件随机 0通过/0失败」，跟超时/代码 bug 分不开，所以必须有秒级用例钉住它。
	ANOTIFY_BIN=$(CURDIR)/anotify node scripts/e2e/harness_selftest.mjs

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

# ---- 纯前端预览（不连后端）----
#
# 只想看前端效果时的最短路径：不需要 server、devseed、WS worker。
# 所有 /v1/* 请求由浏览器里的 web/mock.js 应答，一个请求都不会真正发出。
#
# 默认带 trycloudflare 隧道（和 make dev 一样），方便直接发给别人看。
# 端口用独立变量：PORT 是 dev 的 8080，复用会跟 dev 抢端口。
MOCK_PORT ?= 7788
# 监听地址。默认只绑回环（不暴露到局域网）；
# 要给同一 WiFi 的手机/别人看就用 make mock MOCK_BIND=0.0.0.0，
# 并按脚本输出的提示放行防火墙端口（sudo ufw allow 7788/tcp）。
MOCK_BIND ?= 127.0.0.1

mock: check-classes sitegen ## 启动 mock 服务器（纯前端 + trycloudflare 隧道）
	@MOCK_PORT=$(MOCK_PORT) MOCK_BIND=$(MOCK_BIND) ./scripts/mock.sh

mock-local: check-classes sitegen ## 同上，但不起隧道（只本机访问）
	@NO_TUNNEL=1 MOCK_PORT=$(MOCK_PORT) MOCK_BIND=$(MOCK_BIND) ./scripts/mock.sh

# fe-preview 保留为同义词：文档和旧习惯都在用它
fe-preview: mock ## mock 的同义词（兼容旧写法）
fe-preview-port: ## 同 mock，指定端口（用法：make mock MOCK_PORT=7789）
	@MOCK_PORT=$(MOCK_PORT) MOCK_BIND=$(MOCK_BIND) ./scripts/mock.sh

integration: ## 集成测试（需服务已在 PORT 运行）
	BASE=http://localhost:$(PORT) ./scripts/integration.sh

e2e: ## 【固化门禁】全量端到端测试（并行模式，不含单测）
	./scripts/e2e/run_all.sh

demo-chain: ## 多跳链路 demo（起 server + 3 个节点 + webhook 触发；验证主流程跑通）
	./scripts/demo/chain.sh

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

# ---- 本地开发环境（docker compose + 可选 Cloudflare 隧道）----
# 与 up/down 的区别：这份从**源码**构建（含 devseed 播种工具）、带隧道 sidecar、
# 默认只监听 127.0.0.1，容器名/卷名都带 -dev 后缀，不会碰已部署实例。

# --env-file 让 .env.dev 参与**变量插值**（env_file: 只给容器注入，不参与插值）
DEV_COMPOSE := docker compose --env-file .env.dev -f docker-compose.dev.yml
DEV_PROFILE := $(if $(TUNNEL),--profile tunnel,)

dev-env: ## 生成 .env.dev（真实 VAPID 密钥；已存在则不动）
	@if [ -f .env.dev ]; then echo ".env.dev 已存在，跳过（要重建请先删除）"; else \
	  cp .env.dev.example .env.dev && \
	  $(MAKE) --no-print-directory _dev-env-keys && \
	  echo "✅ .env.dev 已生成"; fi

_dev-env-keys:
	@tmp=$$(mktemp) && GOPROXY=$${GOPROXY:-direct} GOSUMDB=sum.golang.org GOTOOLCHAIN=auto \
	  go run ./scripts/genkeys.go > $$tmp && \
	  pub=$$(grep '^ANOTIFY_VAPID_PUBLIC_KEY=' $$tmp | cut -d= -f2-) && \
	  priv=$$(grep '^ANOTIFY_VAPID_PRIVATE_KEY=' $$tmp | cut -d= -f2-) && \
	  sed -i "s|^ANOTIFY_VAPID_PUBLIC_KEY=.*|ANOTIFY_VAPID_PUBLIC_KEY=$$pub|" .env.dev && \
	  sed -i "s|^ANOTIFY_VAPID_PRIVATE_KEY=.*|ANOTIFY_VAPID_PRIVATE_KEY=$$priv|" .env.dev && \
	  rm -f $$tmp && echo "  VAPID 密钥已写入 .env.dev"

dev-up: dev-env ## 构建并启动开发环境（TUNNEL=1 同时起 Cloudflare 隧道）
	$(DEV_COMPOSE) $(DEV_PROFILE) up -d --build
	@echo "  本地: http://localhost:$${ANOTIFY_DEV_PORT:-8080}"
	@echo "  日志: make dev-logs"
	@echo "  播种: make dev-seed（或 make dev-demo 跑完整链路）"

dev-down: ## 停止开发环境（数据卷保留）
	$(DEV_COMPOSE) --profile tunnel --profile tools down

dev-reset: ## 停止开发环境并删除数据卷（重新开始）
	$(DEV_COMPOSE) --profile tunnel --profile tools down -v
	@echo "  数据卷 anotify-dev-data 已删除"

dev-logs: ## 跟踪开发环境日志（S=服务名，默认 anotify）
	$(DEV_COMPOSE) logs -f $(or $(S),anotify)

dev-seed: ## 播种 demo 用户（幂等）
	$(DEV_COMPOSE) --profile tools run --build --rm seed

dev-demo: ## 播种 + 真跑一条完整事件链（能力图/节点 WS/派发）
	$(DEV_COMPOSE) --profile tools run --build --rm demo

dev-sh: ## 进开发容器（调试）
	$(DEV_COMPOSE) exec anotify sh

dev-tunnel-setup: ## 一次性：建命名隧道 + 配 DNS + 写本地 ingress（需先 cloudflared tunnel login）
	@command -v cloudflared >/dev/null || { echo "缺 cloudflared（https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/）"; exit 1; }
	@test -f $$HOME/.cloudflared/cert.pem || { echo "未登录：先跑 cloudflared tunnel login"; exit 1; }
	@TUNNEL=$${TUNNEL:-anotify-dev}; DOMAIN=$${DOMAIN:-anotify-dev.openaaas.org}; \
	  if ! cloudflared tunnel list 2>/dev/null | grep -qE "$$TUNNEL\b"; then \
	    echo "建隧道 $$TUNNEL …"; cloudflared tunnel create $$TUNNEL; \
	  else echo "隧道 $$TUNNEL 已存在"; fi; \
	  TID=$$(cloudflared tunnel list 2>/dev/null | awk -v n="$$TUNNEL" '$$2==n{print $$1}'); \
	  echo "tunnel id = $$TID"; \
	  cloudflared tunnel route dns $$TUNNEL $$DOMAIN 2>&1 | tail -1; \
	  mkdir -p .dev-tunnel; \
	  printf '%s\n' \
	    "# .dev-tunnel/config.yml —— 本地开发隧道的 ingress（make dev-tunnel-setup 生成，不入库）" \
	    "tunnel: $$TID" \
	    "credentials-file: /etc/cloudflared/credentials.json" \
	    "no-autoupdate: true" \
	    "" \
	    "ingress:" \
	    "  - hostname: $$DOMAIN" \
	    "    service: http://anotify:8080" \
	    "    originRequest:" \
	    "      connectTimeout: 10s" \
	    "  - service: http_status:404" > .dev-tunnel/config.yml; \
	  echo "✅ 已写 .dev-tunnel/config.yml（$$DOMAIN → http://anotify:8080）"; \
	  echo "   启动：make dev-up TUNNEL=1"

dev-tunnel-quick: ## 临时隧道（随机 trycloudflare.com 域名，验证隧道链路用）
	@echo "⚠️  临时隧道域名随机，Passkey 用不了（RP_ID 不匹配）；固定域名见 docs/dev-docker.md"
	docker run --rm -it --network anotify-dev_default cloudflare/cloudflared:latest \
	  tunnel --no-autoupdate --url http://anotify:8080

tunnel: ## Cloudflare 命名隧道（固定域名 dev.openaaas.org）
	cloudflared tunnel run anotify

tunnel-url: ## 读取隧道公网地址（固定 dev.openaaas.org）
	@echo https://dev.openaaas.org

keys: ## 生成 VAPID 密钥对
	go run ./scripts/genkeys.go

clean: ## 清理构建产物
	rm -rf dist anotify internal/server/dist
