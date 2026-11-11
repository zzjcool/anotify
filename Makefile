# Anotify · 构建 / 测试 / 运行
# Go 依赖直连（不使用镜像代理）
export GOPROXY    := direct
export GOSUMDB    := sum.golang.org
export GOTOOLCHAIN := auto

PORT ?= 8080

# 构建版本元数据（契约 §1）：注入 server/devseed 的 internal/version，以及节点
# 二进制 anode 的 main.version / main.commit（两处变量不同源，必须由同一份构建事实注入）。
#
#   VERSION   语义化版本（缺省 git describe --tags --always --dirty；可 make VERSION=1.5.0 覆盖）
#   GIT_SHA   git short sha
#   BUILT_AT  构建时间（RFC3339，UTC）
#
# 三个值都会被塞进 -ldflags，而 linker 按**空格**拆参数：所以它们一律不许带空格。
# 空仓 / 非 git 目录下 git describe 与 git rev-parse 都失败 → 空，VERSION 回落 dev
# （internal/version 的缺省也是 dev），构建照跑。
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := github.com/zzjcool/anotify/internal/version
BUILD_VERSION := $(or $(VERSION),dev)

# **节点与中枢必须报同一个版本串**：中枢拿 internal/version.String()（= Version+Commit）
# 当升级目标版本（internal/server/version_seam.go），节点用 main.version 在 hello 里上报
# 自己（cmd/anode/internal/app）；两者不等 → 「已是最新」永远判不出来，节点会被反复
# 升级。所以 anode 的 main.version 注入的是内部同一份 String() 形状的标签。
VERSION_LABEL := $(BUILD_VERSION)$(if $(GIT_SHA),+$(GIT_SHA))
VERSION_LDFLAGS := -X $(VERSION_PKG).Version=$(BUILD_VERSION)
VERSION_LDFLAGS += -X $(VERSION_PKG).Commit=$(GIT_SHA)
VERSION_LDFLAGS += -X $(VERSION_PKG).BuiltAt=$(BUILT_AT)
ANODE_LDFLAGS := -X main.version=$(VERSION_LABEL)
ANODE_LDFLAGS += -X main.commit=$(or $(GIT_SHA),unknown)

# 节点二进制的输出名（与 scripts/install-anode.sh 下载的发布资产同名：anode）。
ANODE_OUT ?= anode

# 临时目录泄漏守卫（#326）：本机 /tmp 是 tmpfs，测试漏下的目录吃的是**内存**。
TMP_GUARD := node scripts/check-tmp-leaks.mjs

.PHONY: help build anode manifest fe sitegen test bench bench-dispatch run dev docker docker-anode docker-run up down push integration tunnel keys dev-env dev-up dev-down dev-reset dev-logs dev-seed dev-demo dev-sh dev-node dev-tunnel-setup dev-tunnel-quick clean check-classes check-tailwind check-anode-skill tmp-leak-guard tailwind ci mock mock-local fe-preview fe-preview-port

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
# v4 只有两个产品二进制：中枢 anotify（server，内嵌前端）与节点 anode。
# VERSION_LDFLAGS / ANODE_LDFLAGS 让两者报同一份构建事实（版本 + commit）。
build: fe ## 构建生产二进制：anotify（server，内嵌指纹前端）+ anode（节点）
	go build -trimpath -ldflags="-s -w $(VERSION_LDFLAGS)" -tags embeddist -o anotify ./cmd/server
	$(MAKE) --no-print-directory anode
	@echo "✅ anotify + $(ANODE_OUT) 已构建（$(VERSION_LABEL)）"

# anode 单独一个目标：它跑在**用户机器**上，不该为了它去构建前端产物（make build 会跑 fe）。
# 中枢镜像把它当作升级源（GET /v1/node/binary，契约 agent.yaml）发给节点，所以必须带
# 同一份版本信息——否则节点升级上来以后 hello 里报的还是 dev，控制台永远显示「过期」。
anode: ## 构建节点二进制 anode（带版本信息；输出名 ANODE_OUT，缺省 ./anode）
	go build -trimpath -ldflags="-s -w $(ANODE_LDFLAGS)" -o $(ANODE_OUT) ./cmd/anode

# 产物对账：issue #334 要求「make build 产物 manifest 可对账」——二进制 sha256、
# git sha、五份 spec 的合并 hash、前端指纹（dist/manifest.json）各一行，便于和
# docs/v4/g4a-build-manifest.md 里的记录比对。
manifest: build ## 打印构建产物对账信息（二进制 sha256 / 版本 / git sha / spec hash / 前端指纹）
	@echo "version:  $(VERSION_LABEL)"
	@echo "git:      $(GIT_SHA)"
	@echo "built_at: $(BUILT_AT)"
	@sha256sum anotify $(ANODE_OUT)
	@printf 'spec:     %s  (api/*.yaml)\n' "$$(cat api/*.yaml | sha256sum | cut -d' ' -f1)"
	@printf 'fe:       %s  (internal/server/dist/manifest.json)\n' "$$(sha256sum internal/server/dist/manifest.json | cut -d' ' -f1)"

test: ## 运行全部单元测试（不含 make e2e / make integration）
	go test ./... -count=1

# contract: 契约 §9 的静态断言。这几条不依赖运行期，放在 CI 里最快。
#
# A2（I1）：schema 里不得出现可执行的命令/脚本概念
# A4（I3）：kind 是推导量，**不得**落库
# A5（I1）：中枢侧任何包不得 import os/exec —— 执行只发生在节点
# A6（I1）：artifacts / deployments 必须有 digest 列（内容寻址）
# A7（I1）：cmd/agent 不得触碰 node_capabilities（归属只能由部署派生）
# openapi：path/security 由 check_openapi.py 查；**字段级**（Go json tag ↔ schema
# properties，双向）由 internal/server/openapi_parity_test.go 查（见本 target 的注释）。
contract: ## CONTRACT.md §9 的静态不变量断言（A2/A4/A5/A6/A7）+ openapi 与 mux 一致性 + openapi 字段级核对
	@set -e; \
	if grep -riE 'argv|shell|script' internal/store/schema.sql >/dev/null; then \
		echo "❌ A2 失败：schema.sql 里出现 argv/shell/script（不变量 I1）"; \
		grep -riE 'argv|shell|script' internal/store/schema.sql; \
		exit 1; \
	fi
	@python3 scripts/check_contract.py
	@python3 scripts/check_openapi.py
	@# 字段级核对（Capability Spec v3 §7 P6）：check_openapi.py 只查 path 与 security，
	@# schema 与 Go 结构体的双向核对在 Go 测试里（要拿 reflect 读 json tag）。
	@# 放在 contract 而不是只靠 ci 的 test 阶段：PR 里最快的那道门就能当场回答
	@# 「我加的这个字段声明了吗」——否则它要等到几分钟后的全量 test 才红。
	@go test ./internal/server/ -run TestOpenAPIParity -count=1
	@# 节点是独立产品（契约 I1）：它必须真的编得过，而不是靠 import 图偶然成立。
	@go build -o /dev/null ./cmd/anode
	@echo "✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过"

# 节点 CLI 的可发现面（#334，v3 #317 的等价迁移）：v4 的节点产品是 anode（serve/install/emit），
# 没有内嵌 skill.md，于是「手册 ↔ 真二进制」的对账落在 docs/v4/anode-cli.md 与
# scripts/install-anode.sh 上：
#   · 手册代码块里的每个 `anode <sub>` 与 `--flag` 必须能在**真二进制**的 -h 里找到
#     （不 grep 源码：flag 的注册形状会重构，-h 才是用户/脚本看到的那份事实）；
#   · `--version` 的单行 JSON 必须与注入值一致，且 install-anode.sh 的版本校验吃得下；
#   · 边界：anode 只依赖 internal/protocol + internal/artifact，不 import 中枢包、不开数据库。
# 故意写错一个 flag → 这里红。
check-anode-skill: ## 校验 anode CLI 与 docs/v4/anode-cli.md / install-anode.sh 一致（防漂移）
	@python3 scripts/check_anode_skill.py

# 临时目录泄漏守卫（#326）：本机 /tmp 是 tmpfs，测试漏下的目录吃的是**内存**。
# 这里只跑「不执行被测代码」的部分（毫秒级）：
#   · --self-test：守卫自己的红/绿/透传自测（守卫坏了要立刻红，而不是假绿）
#   · --lint：静态拦截 e2e 里的裸 mkdtempSync（新套件不许绕过 H.mkdtempTmp）
# 动态守卫（包住 $(MAKE) test）放在 ci 里，避免把单测再跑一遍。
tmp-leak-guard: ## 临时目录泄漏守卫：自测 + 静态检查（毫秒级）
	$(TMP_GUARD) --self-test
	$(TMP_GUARD) --lint

ci: contract ## PR 门禁：契约静态断言、gofmt、前端 embed 产物、vet、build、单元测试、基准冒烟（不含 e2e / integration）
	@unformatted=$$(git ls-files '*.go' | xargs -r gofmt -l 2>/dev/null); \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "未通过 gofmt，请执行 gofmt -w：" "$$unformatted"; \
		exit 1; \
	fi
	$(MAKE) fe
	go vet ./...
	go build ./...
	@# 节点 CLI 的可发现面（#334）：手册/安装脚本里的子命令与 flag 必须能在真 anode 里找到，
	@# 且 --version 的 JSON 与注入值一致。写错一个 flag，这里就红。
	$(MAKE) check-anode-skill
	go build -trimpath -ldflags="-s -w $(VERSION_LDFLAGS)" -tags embeddist -o anotify ./cmd/server
	go build -trimpath -ldflags="-s -w $(ANODE_LDFLAGS)" -o $(ANODE_OUT) ./cmd/anode
	@# 单测外面套一层临时目录守卫（#326）：给被包命令一个私有 TMPDIR，跑完
	@# 断言这个私有目录里没有 anotify-* 残留。用归因隔离而不是直接比对 /tmp ——
	@# 本机可能同时有别的 e2e / 测试在跑（并行子 agent、另一个 worktree 的
	@# make ci），集合差会把别人漏的算到本次头上（实测发生过）。
	$(MAKE) tmp-leak-guard
	$(TMP_GUARD) -- $(MAKE) test
	@# 基准冒烟（#315 ③）：基准烂掉比测试烂掉更隐蔽——没人跑，坏了也没人知道，
	@# 等真要用时拿到的是错数（#264 就撞过一次：夹具缺 EntryFromKey/EntryFromPort，
	@# 基准静默一条都派不出去，还在打印正常的 ns/op）。
	@# 这里固定 1x：只证明「编译得过 + 能跑出数字 + 夹具仍能真的派出任务」；
	@# 真基准（拿数字对比）仍手动跑 `make bench-dispatch` / `make bench`。
	$(MAKE) bench-dispatch
	@# harness 自测（#215）：/health 的 boot 身份校验是"看不见的"修复——正常跑什么都不打印。
	@# 而这个缺陷的症状是「套件随机 0通过/0失败」，跟超时/代码 bug 分不开，所以必须有秒级用例钉住它。
	ANOTIFY_BIN=$(CURDIR)/anotify node scripts/e2e/harness_selftest.mjs
	@# 安装自测（#338）：v3 的 scripts/install_agent_selftest.sh（install-agent.sh + cmd/agent）
	@# **已删除** —— 它的被测对象 cmd/agent 随 v3 节点产品退役，脚本第一步就要编
	@# 那个已不存在的二进制（必红），留着只是让 `grep cmd/agent` 多一条假线索。
	@# v4 的安装面是 scripts/install-anode.sh（只下载/校验，安装逻辑在 cmd/anode install），由
	@# `go test ./cmd/anode/...` 与 `make check-anode-skill` 覆盖。

# dispatch 全链基准（v4）：真 store + 真 route 投影 + 真 source.Ingest + assign/grant/complete。
# 平时不在任何必跑路径上（`go test ./...` 不跑 Benchmark），所以给它一条命令，否则它只会
# 安静地腐烂（#315 ③）。issue #338 里它就是这么烂掉的：v3 形状 + `dispatchbench` build tag，
# 全仓唯一跑它的地方是这里，而 `go test ./...` 看不见它。现在**没有 build tag** 了——
# 这个基准与它的护栏用例都在 `go test ./internal/store` 里正常编译/运行。
bench-dispatch: ## 跑 dispatch 全链基准（1x：只保证编译+能跑出数字；要数字自己加大 -benchtime）
	go test ./internal/store -run '^$$' -bench BenchmarkDispatchChain -benchtime 1x -benchmem

bench: ## 运行全部基准测试（吞吐/分配热点；仅基准不跑单测）
	go test ./... -run=NONE -bench=. -benchmem -benchtime=1s

bench-report: ## 运行基准并输出到 bench.txt（留存对比基线）
	go test ./... -run=NONE -bench=. -benchmem -benchtime=1s > bench.txt 2>&1
	@echo "基准结果已写入 bench.txt"

run: build ## 本地运行（需先设置 ANOTIFY_VAPID_* 环境变量）
	./anotify

# dev 的真实入口只有三个（v4）：cmd/server（中枢）、cmd/devseed（播种/示例链路）、
# cmd/anode（节点）。cmd/agent 已随 v3 退役，任何目标都不得再引用它。
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

# v3 的 `demo-chain`（起 server + 3 个 cmd/agent 节点 + 多跳 emit）随 cmd/agent 退役：
# v4 的开发链路是 `make dev-demo`（devseed 用真实资源 + 真 WS hello 播种，见 dev.sh）。

e2e-parallel: ## 全量端到端测试（显式并行模式）
	./scripts/e2e/run_all.sh

e2e-serial: ## 全量端到端测试（串行模式，调试用）
	./scripts/e2e/run_all.sh --serial

e2e-one: ## 只跑某个 E2E 套件：make e2e-one S=auth_flow
	./scripts/e2e/run_all.sh $(S)

# e2e-quick：合并前的快检（~25s），覆盖「派发能到 + 部署能对账 + 目录能生效 + 契约没漂」。
#
# 为什么是这四个：
#   live_agent  真 agent 跑真任务，到文件系统验收（专抓"每层都对、连起来才错"）
#   ws_protocol 派发链路 + 帧形状（含 assign 帧字段集合）
#   deploy      部署状态机 / 产物 digest / 本地变体
#   contract    API 形状没漂（契约静态断言 + openapi）
# 全量 e2e（含浏览器套件，约 3.5min）留给打 tag / 合并前最后一道。
e2e-quick: ## 合并前快检（~25s）：live_agent + ws_protocol + deploy + contract
	./scripts/e2e/run_all.sh live_agent ws_protocol deploy contract

docker: ## 构建中枢镜像（server + 内置 anode 升级源）
	docker build -t anotify \
	  --build-arg VERSION=$(BUILD_VERSION) \
	  --build-arg GIT_SHA=$(GIT_SHA) \
	  --build-arg BUILT_AT=$(BUILT_AT) .

# docker-anode 构建节点侧镜像。用法（容器即节点，配置挂 /etc/anode/anode.yaml）：
#   docker run -d --restart=always -v $PWD/anode.yaml:/etc/anode/anode.yaml:ro \
#     -v anode-data:/var/lib/anode anotify-anode
#
# 与中枢镜像分开是刻意的：anode 不需要 web/dist（少一个量级的体积），
# 而且它跑在**用户的**机器上，不该把中枢的前端产物运过去。
docker-anode: ## 构建节点侧 Docker 镜像（anode 单二进制）
	docker build -f Dockerfile.anode -t anotify-anode \
	  --build-arg VERSION=$(VERSION_LABEL) \
	  --build-arg GIT_SHA=$(GIT_SHA) .

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
	  --build-arg VERSION=$(BUILD_VERSION) \
	  --build-arg GIT_SHA=$(GIT_SHA) \
	  --build-arg BUILT_AT=$(BUILT_AT) \
	  -t ghcr.io/zzjcool/anotify:latest \
	  --push .

# ---- 本地开发环境（docker compose + 可选 Cloudflare 隧道）----
# 与 up/down 的区别：这份从**源码**构建（含 devseed 播种工具）、带隧道 sidecar、
# 默认只监听 127.0.0.1，容器名/卷名都带 -dev 后缀，不会碰已部署实例。

# --env-file 让 .env.dev 参与**变量插值**（env_file: 只给容器注入，不参与插值）
DEV_COMPOSE := docker compose --env-file .env.dev -f docker-compose.dev.yml

# 开发镜像的版本注入（CONTRACT-agent-upgrade §1）：compose 的 build.args 读这三个
# 环境变量。export 让所有 dev 目标（dev-up / dev-reset / dev-docker）都带上，
# 否则镜像里 /app/anode 报 dev —— 控制台就会一直提示"节点过期"。
export ANOTIFY_VERSION := $(or $(VERSION),dev)
export ANOTIFY_GIT_SHA := $(GIT_SHA)
export ANOTIFY_BUILT_AT := $(BUILT_AT)
# 节点镜像的版本标签（= 中枢 internal/version.String() 同一个串，见 ANODE_LDFLAGS）。
export ANOTIFY_ANODE_VERSION := $(VERSION_LABEL)
DEV_PROFILE := $(if $(TUNNEL),--profile tunnel,)
# 重建（KEEP_DATA=1）时：显式 TUNNEL=1，或者隧道容器本来就在（说明这台机器配过隧道）
# 都要把它一起拉起来。不这么做的话「重建完公网 1033」这个坑会反复踩。
DEV_PROFILE_KEEP := $(if $(TUNNEL),--profile tunnel,$(shell docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx anotify-dev-tunnel && echo --profile tunnel))

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
	$(DEV_COMPOSE) --profile tunnel --profile tools --profile node down

# 破坏性操作必须**显式**。原来 `dev-reset` 默认 `down -v`，于是「代码改了我重启一下」
# 这一最常见的心智会把账号/节点/事件全删掉（真实踩过，而且是删完才发现）。
#
# 现在：
#   make dev-reset            → 重建镜像 + 容器，**数据卷保留**（默认安全）
#   make dev-reset WIPE=1     → 连数据卷一起删（从零开始）
#   make dev-wipe             → 同上，动作名字直说后果
dev-reset: ## 重建开发环境（默认保留数据；WIPE=1 连数据卷一起删）
ifeq ($(WIPE),1)
	$(DEV_COMPOSE) --profile tunnel --profile tools --profile node down -v
	@echo "  数据卷 anotify-dev-data / anotify-dev-node-data 已删除（WIPE=1）"
else
	$(DEV_COMPOSE) --profile tunnel --profile tools --profile node down
	@# build 必须**显式**跑一次：`up -d --build` 在容器已存在时可能既不重建镜像
	@# 也不重建容器（实测过：pull 到新代码后跑 dev-reset，镜像还是 1 小时前那个，
	@# 容器报 Healthy —— 于是"更新到最新"静默失败，界面还是旧的）。
	@# --force-recreate 再保一道：容器一定用新镜像重新起。
	$(DEV_COMPOSE) build anotify
	$(DEV_COMPOSE) $(DEV_PROFILE_KEEP) up -d --force-recreate
	@echo "  数据卷 anotify-dev-data 已保留"
	@echo "  本地: http://localhost:$${ANOTIFY_DEV_PORT:-8080}"
	@echo "  要清空数据（账号/节点/事件全删）：make dev-wipe"
endif

# dev-wipe 是 dev-reset WIPE=1 的别名：破坏性操作的入口不该藏在参数里。
# 删之前先把要删的东西说清楚——「删完才发现」是最贵的一类事故。
dev-wipe: ## ⚠️ 停止开发环境并删除数据卷（账号/节点/事件全部丢失）
	@echo "即将删除数据卷 anotify-dev-data / anotify-dev-node-data（命名空间、账号、passkey、节点、事件、任务）"
	$(DEV_COMPOSE) --profile tunnel --profile tools --profile node down -v
	@echo "  数据卷 anotify-dev-data / anotify-dev-node-data 已删除"

dev-logs: ## 跟踪开发环境日志（S=服务名，默认 anotify）
	$(DEV_COMPOSE) logs -f $(or $(S),anotify)

dev-seed: ## 播种 demo 用户（幂等）
	$(DEV_COMPOSE) --profile tools run --build --rm seed

dev-demo: ## 播种 + 真跑一条完整事件链（能力图/节点 WS/派发）
	$(DEV_COMPOSE) --profile tools run --build --rm demo

# 节点容器（可选）：真正跑能力的 anode 服务。config 挂 /etc/anode/anode.yaml（含真实
# name/server/token，token 由中枢 POST /v1/nodes + /tokens 签发），数据卷独立于中枢。
dev-node: ## 在开发环境里起一台真节点（需要 .dev-anode/anode.yaml）
	@test -f "$${ANODE_CONFIG:-.dev-anode/anode.yaml}" || { \
	  echo "缺少节点配置：$${ANODE_CONFIG:-.dev-anode/anode.yaml}"; \
	  echo "  1) 建节点 + 签 token：POST /v1/nodes 与 POST /v1/nodes/{name}/tokens"; \
	  echo "  2) 写 anode.yaml（name/server: ws://anotify:8080/token/namespaces）"; \
	  echo "  3) 再跑 make dev-node"; exit 1; }
	$(DEV_COMPOSE) --profile node up -d anode
	@echo "  日志：make dev-logs S=anode"

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
	rm -rf dist anotify $(ANODE_OUT) server agent sitegen internal/server/dist
