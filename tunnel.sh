#!/usr/bin/env bash
# ==============================================================
# 本地服务 + Cloudflare Tunnel 暴露到公网的管理脚本
# 用法: ./tunnel.sh <command> [PORT] [DIR]
# 命令: start | serve | tunnel | url | stop | clean | help
# ==============================================================
set -u

# ---------- 配置 ----------
PORT=${2:-5699}
DIR=${3:-.}
LOG_DIR="/tmp/anotify"
SERVE_LOG="$LOG_DIR/serve.log"
TUNNEL_LOG="$LOG_DIR/tunnel.log"

mkdir -p "$LOG_DIR"

get_url() {
    grep -oE "https://[a-z0-9-]+\.trycloudflare\.com" "$TUNNEL_LOG" 2>/dev/null | tail -1
}

# 检测真实 cloudflared 隧道进程（排除 grep/bash 自身）
tunnel_running() {
    pgrep -f "cloudflared tunnel --url" > /dev/null 2>&1 || \
    pgrep -x cloudflared > /dev/null 2>&1
}

cmd_help() {
    cat <<EOF
用法: ./tunnel.sh <command> [PORT] [DIR]
命令:
  start    一键启动本地服务 + 隧道   (默认 PORT=5699, DIR=当前目录)
  serve    仅启动本地 HTTP 服务
  tunnel   仅启动 Cloudflare 隧道
  url      显示当前公网地址
  stop     停止所有相关进程
  clean    停止进程并清理日志
示例:
  ./tunnel.sh start 8000 ./public
EOF
}

cmd_serve() {
    echo ">>> 启动本地服务 (端口 $PORT, 目录 $DIR)"
    if lsof -ti :$PORT > /dev/null 2>&1; then
        echo "    端口 $PORT 已被占用，跳过启动"
    else
        nohup python3 -m http.server "$PORT" --directory "$DIR" > "$SERVE_LOG" 2>&1 &
        echo "    HTTP server PID: $!"
        sleep 1
    fi
    curl -s -o /dev/null -w "    http://localhost:$PORT  -> HTTP %{http_code}\n" "http://localhost:$PORT"
}

cmd_tunnel() {
    echo ">>> 启动 Cloudflare 隧道 -> localhost:$PORT"
    if tunnel_running; then
        echo "    隧道已在运行"
    else
        nohup cloudflared tunnel --url "http://localhost:$PORT" > "$TUNNEL_LOG" 2>&1 &
        echo "    cloudflared PID: $!"
        sleep 8
    fi
    URL=$(get_url)
    if [ -n "$URL" ]; then
        echo ">>> 公网地址: $URL"
    else
        echo ">>> 公网地址尚未生成，稍后运行 ./tunnel.sh url 查看"
    fi
}

cmd_start() {
    cmd_serve
    cmd_tunnel
    echo ""
    echo ">>> 公网验证:"
    URL=$(get_url)
    if [ -n "$URL" ]; then
        curl -s -o /dev/null -w "    $URL -> HTTP %{http_code}\n" "$URL"
    else
        echo "    (URL 未生成)"
    fi
}

cmd_url() {
    URL=$(get_url)
    if [ -n "$URL" ]; then
        echo "公网地址: $URL"
    else
        echo "未找到公网地址，请先运行 ./tunnel.sh start 或 ./tunnel.sh tunnel"
    fi
}

cmd_stop() {
    echo ">>> 停止 cloudflared 隧道"
    pkill -f "cloudflared tunnel --url" 2>/dev/null
    pkill -x cloudflared 2>/dev/null
    sleep 1
    if tunnel_running; then
        echo "    仍有残留进程"
    else
        echo "    已停止"
    fi
    echo ">>> 停止端口 $PORT 上的服务"
    lsof -ti :$PORT | xargs kill 2>/dev/null && echo "    已停止" || echo "    未运行"
}

cmd_clean() {
    cmd_stop
    echo ">>> 清理日志目录 $LOG_DIR"
    rm -rf "$LOG_DIR" && echo "    已清理"
}

# ---------- 分发命令 ----------
case "${1:-}" in
    start)  cmd_start ;;
    serve)  cmd_serve ;;
    tunnel) cmd_tunnel ;;
    url)    cmd_url ;;
    stop)   cmd_stop ;;
    clean)  cmd_clean ;;
    help|-h|--help|"") cmd_help ;;
    *) echo "未知命令: $1"; cmd_help ;;
esac
