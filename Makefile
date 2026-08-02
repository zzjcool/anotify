# ==============================================================
# 本地端口通过 Cloudflare Tunnel 暴露到公网
# ==============================================================
# 常用命令：
#   make start    一键启动本地服务 + 隧道（推荐）
#   make serve    仅启动本地 HTTP 服务
#   make tunnel   仅启动 Cloudflare 隧道
#   make url      显示当前公网地址
#   make stop     停止所有相关进程
#   make clean    停止进程并清理日志
#
# 常用变量覆盖（示例）：
#   make start PORT=8000 DIR=./public
# ==============================================================

PORT ?= 5699          # 本地服务端口
DIR  ?= .             # 本地服务的根目录
SCRIPT := ./tunnel.sh # 管理脚本

.PHONY: help start serve tunnel url stop clean

help:
	@$(SCRIPT) help

start:
	@$(SCRIPT) start $(PORT) $(DIR)

serve:
	@$(SCRIPT) serve $(PORT) $(DIR)

tunnel:
	@$(SCRIPT) tunnel $(PORT)

url:
	@$(SCRIPT) url

stop:
	@$(SCRIPT) stop $(PORT)

clean:
	@$(SCRIPT) clean $(PORT)
