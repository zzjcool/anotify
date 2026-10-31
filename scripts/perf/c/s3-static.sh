#!/usr/bin/env bash
# C3 · 静态资源缓存评估：Cache-Control 分级 / ETag / 304 / gzip
#
# 只评估、不改产品代码。做的事：
#   1. pc_build（make fe → internal/server/dist 指纹产物）+ pc_start（-tags embeddist，真实文件名）
#   2. 从 `curl /` 的 HTML 里**抽取**真实资源 URL（不硬编码文件名）
#   3. 对每个对象做 6 组探测：全头 dump / 状态·耗时·字节 / If-None-Match / If-Modified-Since
#      / Accept-Encoding / 本地 gzip 估算压缩率
#   4. 产出 raw/headers.txt（全量原始输出）、raw/table.tsv|table.md（汇总表）、finding.md
#
# 用法：bash scripts/perf/c/s3-static.sh
#   端口 7942（撞了顺延 7947，契约 §3 的重试位）；分核 taskset -c 8-11；
#   产物 ${PC_ROOT:-/tmp/perf-c}/s3/
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "$SCRIPT_DIR/common.sh"

OUT="$PC_ROOT/s3"
RAW="$OUT/raw"
HDR="$RAW/headers.txt"
TAB="$RAW/table.tsv"
MDT="$RAW/table.md"
mkdir -p "$RAW"
: >"$HDR"
: >"$TAB"
: >"$MDT"
: >"$RAW/gzip-estimate.tsv"

CURL=(taskset -c "$PC_CPUS" curl)

log() { printf '%s\n' "$*" >>"$HDR"; }
hr() {
	log ""
	log "=================================================================================="
	log "## $*"
	log "=================================================================================="
}

# final_headers：从 curl -I -L 的多跳输出里取**最后一跳**的头块。
final_headers() { tr -d '\r' | awk '/^HTTP\//{blk=$0 ORS; next} {blk=blk $0 ORS} END{printf "%s", blk}'; }
# hv <头块文本> <Header-Name>：取该头的最后一个值（去掉「名字: 」前缀）。
hv() { printf '%s' "$1" | tr -d '\r' | grep -i "^$2:" | tail -1 | sed -E 's/^[^:]+:[[:space:]]*//; s/[[:space:]]+$//'; }

# head_final <url>：最终跳的响应头（文本）。
head_final() { "${CURL[@]}" -sS -I -L --max-time 5 "$1" 2>&1 | final_headers; }

# getm <curl args...>：打一行「码 \t 耗时 \t 字节 \t 生效URL」，头 dump 到 -D 指定的文件。
getm() {
	"${CURL[@]}" -sS -o /dev/null --max-time 10 \
		-w '%{http_code}\t%{time_total}\t%{size_download}\t%{url_effective}' "$@" 2>&1
}

# probe_get <dumpfile> <curl args...>：打点 + 记录响应头，stdout 回显指标行。
probe_get() {
	local dump="$1"
	shift 1
	local m
	m=$(getm -D "$dump" "$@")
	log "  request : curl -sS -o /dev/null -D $(basename "$dump") $*"
	log "  result  : $m   [code<TAB>time_total<TAB>size_download<TAB>url_effective]"
	if [ -s "$dump" ]; then
		log "  resp-hdr:"
		tr -d '\r' <"$dump" | sed 's/^/      /' >>"$HDR"
	fi
	printf '%s' "$m"
}

# ---------------------------------------------------------------------------
# 0. 环境
# ---------------------------------------------------------------------------
pc_require_env || exit 1
pc_build || exit 1

pick_port() {
	local p
	for p in 7942 7947; do # 7947 = 契约 §3 给的重试位；两个都被占就放弃，不抢别人的端口
		if ! ss -lnt 2>/dev/null | grep -qE "[:.]${p}[[:space:]]"; then
			printf '%s' "$p"
			return 0
		fi
	done
	return 1
}
PORT="$(pick_port)" || {
	printf 's3: 7942/7947 都被占用，放弃（不抢别人的端口）\n' >&2
	exit 1
}
hr "环境"
log "repo      : $PC_REPO"
log "bin       : $PC_BIN"
log "cpus      : $PC_CPUS"
log "port      : $PORT"
log "date      : $(date -Is)"
log "uname     : $(uname -srm)"
log "go        : $(go version 2>/dev/null || echo '?')"
pc_start static "$PORT" >/dev/null || exit 1
trap 'pc_stop' EXIT
log "base      : $PC_BASE   (pid $(cat "$PC_RUN/server.pid"))"
# embed 模式确认：dist 指纹文件名确实是内嵌产物
log ""
log "--- internal/server/dist 指纹产物（pc_build 的 make fe 输出，取前 12 个）---"
ls -1 "$PC_REPO/internal/server/dist" 2>/dev/null | head -12 | sed 's/^/    /' >>"$HDR"

# ---------------------------------------------------------------------------
# 1. 从 HTML 抽取真实资源 URL（不硬编码）
# ---------------------------------------------------------------------------
hr "1. 从 GET / 的 HTML 抽取真实资源 URL"

fetch() { "${CURL[@]}" -sS --max-time 8 "$1" 2>&1; }

HTML_ROOT="$(fetch "$PC_BASE/")"
printf '%s' "$HTML_ROOT" >"$RAW/html-index.html"
log "GET $PC_BASE/ → $(printf '%s' "$HTML_ROOT" | wc -c) 字节 HTML，落 raw/html-index.html"

# all_refs <html>：去重的 src=/href= 值（保持出现顺序）
all_refs() {
	printf '%s' "$1" | grep -oE '(src|href)="[^"#?]+"' |
		sed -E 's/^(src|href)="//; s/"$//' | awk '!seen[$0]++'
}
# local_refs <html>：只保留同源（/ 开头或相对），丢掉 http(s):// 与 // 外部资源
local_refs() { all_refs "$1" | grep -vE '^(https?:)?//'; }
# to_abs <ref>：相对 HTML 根（/）解析成绝对 URL
to_abs() { case "$1" in /*) printf '%s%s' "$PC_BASE" "$1" ;; *) printf '%s/%s' "$PC_BASE" "$1" ;; esac; }
# clen <url>：Content-Length（拿不到就是 0）
clen() {
	local v
	v=$(head_final "$1" | grep -i '^content-length:' | tail -1 | tr -dc '0-9')
	printf '%s' "${v:-0}"
}
# pick_largest <grep-regex>：在同源引用里按 Content-Length 取最大者（避免挑到 3KB 的空壳）
pick_largest() {
	local best="" bestlen=-1 u l
	while IFS= read -r u; do
		[ -z "$u" ] && continue
		l=$(clen "$(to_abs "$u")")
		if [ "$l" -gt "$bestlen" ] 2>/dev/null; then best="$u" bestlen="$l"; fi
	done < <(local_refs "$HTML_ROOT" | grep -E "$1")
	printf '%s' "$best"
}

log ""
log "--- HTML 里的全部同源引用 ---"
local_refs "$HTML_ROOT" | sed 's/^/    /' >>"$HDR"
log ""
log "--- 外部（跨域）引用，不纳入本场景 ---"
all_refs "$HTML_ROOT" | grep -E '^(https?:)?//' | sed 's/^/    /' >>"$HDR" || true

JS_PATH="$(pick_largest '\.js$')"
CSS_PATH="$(pick_largest '\.css$')"
IMG_PATH="$(pick_largest '\.(svg|png|ico)$')"
MANIFEST_PATH="$(local_refs "$HTML_ROOT" | grep -E 'manifest\.(webmanifest|json)$' | head -1)"

# 字体：从「字体 CSS」里抽 url(...)（字体不在 HTML 里，必须二次解析 CSS）
FONTS_CSS_PATH="$(local_refs "$HTML_ROOT" | grep -E '\.css$' | grep -iE 'font' | head -1)"
[ -z "$FONTS_CSS_PATH" ] && FONTS_CSS_PATH="$CSS_PATH"
FONTS_CSS_BODY="$(fetch "$(to_abs "$FONTS_CSS_PATH")")"
printf '%s' "$FONTS_CSS_BODY" >"$RAW/fonts-css.body"
FONT_REF="$(printf '%s' "$FONTS_CSS_BODY" | grep -oE "url\(['\"]?[^)]+\)" |
	sed -E "s/^url\(['\"]?//; s/['\"]?\)$//" | grep -E '\.(woff2?|ttf|otf)$' | head -1)"
if [ -n "$FONT_REF" ]; then
	case "$FONT_REF" in
	/*) FONT_PATH="$FONT_REF" ;;
	*) FONT_PATH="$(dirname "$FONTS_CSS_PATH")/$FONT_REF" ;;
	esac
else
	FONT_PATH=""
fi

# sw.js：Service Worker 注册路径写在 JS 里（不在 HTML）→ 从主 JS 正文里抽；兜底 /sw.js
JS_BODY=""
[ -n "$JS_PATH" ] && JS_BODY="$(fetch "$(to_abs "$JS_PATH")")"
printf '%s' "$JS_BODY" >"$RAW/main-js.body"
SW_PATH="$(printf '%s\n%s' "$HTML_ROOT" "$JS_BODY" | grep -oE "['\"\`][^'\"\`]*sw\.js['\"\`]" | head -1 | tr -d "'\"\`")"
[ -z "$SW_PATH" ] && SW_PATH="/sw.js"

log ""
log "--- 抽取结果（全部来自 HTML/CSS 正文，非硬编码）---"
log "  JS (最大)      : $JS_PATH"
log "  CSS (最大)     : $CSS_PATH"
log "  图片 (最大)    : $IMG_PATH"
log "  字体 CSS       : $FONTS_CSS_PATH"
log "  字体文件       : $FONT_PATH"
log "  Service Worker : $SW_PATH"
log "  manifest       : $MANIFEST_PATH"
log ""

# 首屏引用统计。必须区分两类，否则「多少个资源要校验」会被 <a href> 导航链接污染：
#   * 子资源（浏览器会自动发起请求）：src= 全部 + href= 里以静态后缀结尾的（css/js/图片/字体/webmanifest）
#   * 导航链接（只有点了才请求）：href= 里的 .html 与无后缀页面
src_vals() { printf '%s' "$HTML_ROOT" | grep -oE 'src="[^"#?]+"' | sed -E 's/^src="//; s/"$//' | awk '!seen[$0]++'; }
href_vals() { printf '%s' "$HTML_ROOT" | grep -oE 'href="[^"#?]+"' | sed -E 's/^href="//; s/"$//' | awk '!seen[$0]++'; }
sub_refs() {
	{
		src_vals
		href_vals | grep -E '\.(css|js|mjs|svg|png|ico|jpe?g|webp|woff2?|ttf|otf|webmanifest|json)$'
	} | grep -vE '^(https?:)?//' | awk '!seen[$0]++'
}
nav_refs() { href_vals | grep -vE '\.(css|js|mjs|svg|png|ico|jpe?g|webp|woff2?|ttf|otf|webmanifest)$' | grep -vE '^(https?:)?//'; }

N_ALL=$(local_refs "$HTML_ROOT" | wc -l | tr -d ' ')
N_SUB=$(sub_refs | wc -l | tr -d ' ')
N_SUB_FIN=$(sub_refs | grep -cE '\.[0-9a-f]{8}\.[A-Za-z0-9]+$')
N_SUB_NOF=$(sub_refs | grep -vE '\.[0-9a-f]{8}\.[A-Za-z0-9]+$' | grep -c .)
N_NAV=$(nav_refs | wc -l | tr -d ' ')
log "  同源引用共 $N_ALL 条：子资源 $N_SUB 条（带指纹 $N_SUB_FIN / 不带指纹 $N_SUB_NOF），导航链接 $N_NAV 条"
log ""
log "--- 子资源清单（浏览器首屏会自动请求这些）---"
sub_refs | sed 's/^/    /' >>"$HDR"
log ""
log "--- 导航链接（只是 <a href>，不会自动请求）---"
nav_refs | sed 's/^/    /' >>"$HDR"
log ""
log "--- 不带指纹的子资源（缓存过期后必须全量重取）---"
sub_refs | grep -vE '\.[0-9a-f]{8}\.[A-Za-z0-9]+$' | sed 's/^/    /' >>"$HDR"

# 首屏字节账：逐个 HEAD 拿 Content-Length 求和；gzip 侧用本地 gzip -9 估算（标注为估算）。
FS_PLAIN=0
FS_GZ=0
FS_ROWS=""
while IFS= read -r ref; do
	[ -z "$ref" ] && continue
	u="$(to_abs "$ref")"
	l="$(clen "$u")"
	local_f="$PC_REPO/internal/server/dist${ref}"
	g=0
	[ -f "$local_f" ] && g=$(gzip -9 -c "$local_f" | wc -c | tr -d ' ')
	FS_PLAIN=$((FS_PLAIN + l))
	FS_GZ=$((FS_GZ + g))
	FS_ROWS="$FS_ROWS$l	$g	$ref\n"
	done < <(sub_refs)
printf '%b' "$FS_ROWS" >"$RAW/firstscreen-bytes.tsv"
FS_PLUS_DOC=$((FS_PLAIN + $(clen "$PC_BASE/")))
log "  首屏合计（HTML + $N_SUB 个子资源，plain）: $FS_PLUS_DOC 字节"
log "  其中子资源 plain=$FS_PLAIN 字节；同内容 gzip -9 估算=$FS_GZ 字节"
awk -v a="$FS_PLAIN" -v b="$FS_GZ" 'BEGIN{if (b>0) printf "    子资源压缩比 plain/gzip9 = %.2fx\n", a/b}' >>"$HDR"

# ---------------------------------------------------------------------------
# 2. 逐对象探测
# ---------------------------------------------------------------------------
# probe_object <key> <path>：6 组探测 + 记一行 TSV/表格。
probe_object() {
	local key="$1" p="$2"
	[ -z "$p" ] && return 0
	local url; url="$(to_abs "$p")"
	hr "OBJECT [$key]  $p   ($url)"

	# --- [1a/1b] 全头 dump：重定向前后各一次 ---
	log "--- [1a] HEAD 不跟随重定向 ---"
	"${CURL[@]}" -sS -I --max-time 5 "$url" 2>&1 | tr -d '\r' | sed 's/^/    /' >>"$HDR"
	log "--- [1b] HEAD 跟随重定向（含每一跳）---"
	local hf; hf="$(head_final "$url")"
	"${CURL[@]}" -sS -I -L --max-time 5 "$url" 2>&1 | tr -d '\r' | sed 's/^/    /' >>"$HDR"

	local status cc etag lm vary ce la
	status="$(printf '%s' "$hf" | head -1 | tr -d '\r')"
	cc="$(hv "$hf" 'Cache-Control')"
	etag="$(hv "$hf" 'ETag')"
	lm="$(hv "$hf" 'Last-Modified')"
	vary="$(hv "$hf" 'Vary')"
	ce="$(hv "$hf" 'Content-Encoding')"
	la="$(hv "$hf" 'Accept-Ranges')"
	log "  >> 最终跳 status=$status"
	log "  >> Cache-Control=${cc:-（无该头）}"
	log "  >> ETag=${etag:-（无该头）}"
	log "  >> Last-Modified=${lm:-（无该头）}"
	log "  >> Vary=${vary:-（无该头）}  Accept-Ranges=${la:-（无该头）}  Content-Encoding=${ce:-（无该头，即 identity）}"

	# --- [2] GET 状态/耗时/字节 ---
	log ""
	log "--- [2a] GET 状态/耗时/字节（不跟随重定向）---"
	local m1; m1="$(probe_get "$RAW/$key.get-nofollow.hdr" "$url")"
	log "--- [2b] GET 状态/耗时/字节（跟随重定向）---"
	local m2; m2="$(probe_get "$RAW/$key.get-follow.hdr" -L "$url")"
	local code1 code2; code1="$(printf '%s' "$m1" | cut -f1)"; code2="$(printf '%s' "$m2" | cut -f1)"
	local bytes2; bytes2="$(printf '%s' "$m2" | cut -f3)"

	# --- [3] If-None-Match ---
	log ""
	local inm_code="-"
	if [ -n "$etag" ]; then
		log "--- [3a] GET If-None-Match: $etag ---"
		inm_code="$(getm -L -H "If-None-Match: $etag" "$url" | cut -f1)"
		log "  request : curl -H 'If-None-Match: $etag' -L $url"
		log "  result  : HTTP $inm_code"
	else
		log "--- [3a] If-None-Match：**响应无 ETag** → 无从发起条件请求（inm=无ETag）---"
		inm_code="无ETag"
	fi
	log "--- [3b] 对照：用一个不存在的 ETag 试探服务端是否真的看 If-None-Match ---"
	local bogus; bogus="$("${CURL[@]}" -sS -o /dev/null -L --max-time 10 \
		-H 'If-None-Match: "s3-probe-does-not-exist"' \
		-w '%{http_code}\t%{size_download}' "$url" 2>&1)"
	log "  request : curl -H 'If-None-Match: \"s3-probe-does-not-exist\"' -L $url"
	log "  result  : $bogus   （304 说明看 INM；200+全字节说明完全忽略 INM）"

	# --- [4] If-Modified-Since ---
	log ""
	local ims_code="-"
	if [ -n "$lm" ]; then
		log "--- [4a] GET If-Modified-Since: $lm ---"
		local r; r="$(getm -L -H "If-Modified-Since: $lm" "$url")"
		ims_code="$(printf '%s' "$r" | cut -f1)"
		log "  request : curl -H 'If-Modified-Since: $lm' -L $url"
		log "  result  : $r"
	else
		log "--- [4a] If-Modified-Since：**响应无 Last-Modified** → 无从发起条件请求（ims=无LM）---"
		ims_code="无LM"
	fi
	log "--- [4b] 对照：带一个「比任何文件都新」的 If-Modified-Since 试探 ---"
	local r2; r2="$("${CURL[@]}" -sS -o /dev/null -L --max-time 10 \
		-H 'If-Modified-Since: Thu, 01 Jan 2099 00:00:00 GMT' -w '%{http_code}\t%{size_download}' "$url" 2>&1)"
	log "  result  : $r2   （304 说明服务端有可用的 modtime 参与了比较）"

	# --- [5] Accept-Encoding ---
	log ""
	log "--- [5] GET Accept-Encoding: gzip, br ---"
	local gz; gz="$("${CURL[@]}" -sS -o /dev/null -L --max-time 10 \
		-D "$RAW/$key.gzip.hdr" -H 'Accept-Encoding: gzip, br' \
		-w '%{http_code}\t%{size_download}' "$url" 2>&1)"
	local gzcode="${gz%%	*}" gzbytes="${gz##*	}"
	local gzce; gzce="$(hv "$(final_headers <"$RAW/$key.gzip.hdr")" 'Content-Encoding')"
	local gzvary; gzvary="$(hv "$(final_headers <"$RAW/$key.gzip.hdr")" 'Vary')"
	log "  result  : code=$gzcode Content-Encoding=${gzce:-（无）} size_download=$gzbytes Vary=${gzvary:-（无）}"

	# --- [6] 本地 gzip -9 估算「本该压成多少」---
	local gz9="-" gz9note=""
	local localfile=""
	case "$p" in
	*/) localfile="$PC_REPO/internal/server/dist/index.html" ;;
	*) localfile="$PC_REPO/internal/server/dist${p}" ;;
	esac
	if [ -f "$localfile" ]; then
		local plain gz9b
		plain=$(wc -c <"$localfile" | tr -d ' ')
		gz9b=$(gzip -9 -c "$localfile" | wc -c | tr -d ' ')
		gz9="$gz9b"
		gz9note="$(awk -v a="$plain" -v b="$gz9b" 'BEGIN{printf "%.2f", a/b}')x"
		log "--- [6] 本地 gzip -9 估算（.gz 体积，非服务端输出）---"
		log "  file=$localfile plain=$plain gzip9=$gz9b 压缩比=${gz9note}（plain/gzip9）"
		printf '%s\t%s\t%s\n' "$p" "$plain" "$gz9b" >>"$RAW/gzip-estimate.tsv"
	fi

	# --- 汇总行 ---
	local h_str="${cc:-—}" e_str="${etag:-无}" l_str="${lm:-无}"
	[ -n "$etag" ] && e_str="有（$(printf '%s' "$etag" | cut -c1-18)…）"
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$key" "$p" "$code1→$code2" "$h_str" "$e_str" "$l_str" "$inm_code" "$ims_code" \
		"${ce:-无}" "$gzbytes" "$bytes2" "${gz9}" "${gz9note:-—}" >>"$TAB"
	printf '| %s | `%s` | %s | %s | %s | %s | %s | %s | %s | %s → %s |\n' \
		"$key" "$p" "$code1→$code2" "$h_str" "$e_str" "$l_str" "$inm_code" "$ims_code" \
		"${ce:-无}" "$bytes2" "$gzbytes" >>"$MDT"
}

probe_object root "/"
probe_object index_html "/index.html"
probe_object events_html "/events.html"
probe_object playbooks_html "/playbooks.html"
probe_object app_js "${JS_PATH:-/}"
probe_object app_css "${CSS_PATH:-/}"
probe_object font "${FONT_PATH:-/}"
probe_object image "${IMG_PATH:-/}"
probe_object sw_js "$SW_PATH"
probe_object manifest "${MANIFEST_PATH:-/}"
probe_object agent_login_sh "/agent-login.sh"
probe_object not_found_txt "/s3-no-such-file.txt"
probe_object not_found_hashed "/ghost.0123abcd.js"
# 重定向跟随的正面样本：目录缺尾斜杠（http.FileServer 的 localRedirect）
probe_object dir_no_slash "/assets"
probe_object en_dir "/en"

# ---------------------------------------------------------------------------
# 3. 补充事实：embed.FS 的 modtime / 首屏字节账 / 404 的 Cache-Control
# ---------------------------------------------------------------------------
hr "3. 归因需要的补充事实"

log "--- internal/server/static.go 里是否有任何地方设置 ETag / gzip ---"
grep -nE 'ETag|Content-Encoding|gzip|Last-Modified|Cache-Control' "$PC_REPO/internal/server/static.go" |
	sed 's/^/    /' >>"$HDR" || log "    （无匹配）"
log "--- 全仓（internal/ + cmd/）gzip/deflate/brotli/压缩中间件 ---"
if grep -rnE 'gzip|deflate|brotli|Compress' "$PC_REPO/internal" "$PC_REPO/cmd" --include='*.go' >>"$HDR" 2>&1; then
	log "    ↑ 有匹配（见上）"
else
	log "    （无任何匹配：没有压缩中间件，也没有 Content-Encoding 相关代码）"
fi
log "--- 工具链版本（Go 1.23+ 的 fs.serveError 会删掉 404 上的 Cache-Control/ETag/Last-Modified）---"
log "    $(go version 2>/dev/null || echo '?')"
grep -n 'httpservecontentkeepheaders' "$(go env GOROOT)/src/net/http/fs.go" | head -5 | sed 's/^/    /' >>"$HDR"
grep -n -A 18 'func serveError' "$(go env GOROOT)/src/net/http/fs.go" | sed 's/^/    /' >>"$HDR"
log "--- embed.FS 的 FileInfo.ModTime()（决定 FileServer 会不会发 Last-Modified）---"
EP="$RAW/embedprobe"
mkdir -p "$EP"
cat >"$EP/main.go" <<'GO'
package main

// 只验证一件事：embed.FS 的 FileInfo.ModTime() 是不是零值
// （Go 标准库 setLastModified 对零时间不写 Last-Modified 头 → embed 模式下静态资源没有校验器）。
import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed probe.txt
var ef embed.FS

func main() {
	fi, err := fs.Stat(ef, "probe.txt")
	if err != nil {
		fmt.Println("stat error:", err)
		return
	}
	fmt.Printf("embed.FS FileInfo.ModTime() = %q  IsZero=%v\n", fi.ModTime(), fi.ModTime().IsZero())
}
GO
echo probe >"$EP/probe.txt"
printf 'module embedprobe\n\ngo 1.22\n' >"$EP/go.mod"
(cd "$EP" && GOTOOLCHAIN=local go run main.go 2>&1 | sed 's/^/    /' >>"$HDR")

log "--- 首屏（/）每次导航必然重复取的对象字节账 ---"
awk -F'\t' '$1=="root"||$1=="sw_js"||$1=="manifest"{printf "    %-10s %-24s plain=%-7s gzip9-est=%-7s (%s)\n", $1, $2, $11, $12, $13}' "$TAB" >>"$HDR"
log "--- 带指纹子资源字节账（本地 gzip -9 估算，raw/firstscreen-bytes.tsv）---"
awk -F'\t' '{printf "    %-9s gzip9-est=%-8s %s\n", $1, $2, $3}' "$RAW/firstscreen-bytes.tsv" >>"$HDR" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 4. finding.md
# ---------------------------------------------------------------------------
ROOT_PLAIN=$(awk -F'\t' '$1=="root"{print $11}' "$TAB")
SW_PLAIN=$(awk -F'\t' '$1=="sw_js"{print $11}' "$TAB")
MAN_PLAIN=$(awk -F'\t' '$1=="manifest"{print $11}' "$TAB")
JS_PLAIN=$(awk -F'\t' '$1=="app_js"{print $11}' "$TAB")
CSS_PLAIN=$(awk -F'\t' '$1=="app_css"{print $11}' "$TAB")
JS_GZ9=$(awk -F'\t' '$1=="app_js"{print $12}' "$TAB")
CSS_GZ9=$(awk -F'\t' '$1=="app_css"{print $12}' "$TAB")
JS_RATIO=$(awk -F'\t' '$1=="app_js"{print $13}' "$TAB")
CSS_RATIO=$(awk -F'\t' '$1=="app_css"{print $13}' "$TAB")
ROOT_GZ9=$(awk -F'\t' '$1=="root"{print $12}' "$TAB")
ROOT_RATIO=$(awk -F'\t' '$1=="root"{print $13}' "$TAB")
NF404_CC=$(awk -F'\t' '$1=="not_found_hashed"{print $4}' "$TAB")
NF404_ST=$(awk -F'\t' '$1=="not_found_hashed"{print $3}' "$TAB")
NF404TXT_CC=$(awk -F'\t' '$1=="not_found_txt"{print $4}' "$TAB")
NF404TXT_ST=$(awk -F'\t' '$1=="not_found_txt"{print $3}' "$TAB")
ALSH_ST=$(awk -F'\t' '$1=="agent_login_sh"{print $3}' "$TAB")
GZ_CE=$(awk -F'\t' '$1=="app_css"{print $9}' "$TAB")
JS_CE=$(awk -F'\t' '$1=="app_js"{print $9}' "$TAB")
N_OBJ=$(wc -l <"$TAB" | tr -d ' ')
GOROOT_ACT=$(go env GOROOT 2>/dev/null || echo '<goroot>')
EV_PLAIN=$(awk -F'\t' '$1=="events_html"{print $11}' "$TAB")
PB_PLAIN=$(awk -F'\t' '$1=="playbooks_html"{print $11}' "$TAB")
EN_PLAIN=$(awk -F'\t' '$1=="en_dir"{print $11}' "$TAB")

{
	cat <<MD
## 静态资源：\`Cache-Control\` 分级生效但**既无 ETag 也无 Last-Modified**（embed 模式），且**没有 gzip**——指纹资源可长期缓存，非指纹资源每次导航全量重传

- 严重度：major（\`gzip\` 缺失 + 条件请求完全失效两项，各自独立成条；本文件按契约 §4 一节一节拆）
- 场景：C3 静态资源缓存（契约 §3）· 分支 \`$(git -C "$PC_REPO" rev-parse --short HEAD 2>/dev/null)\` · $(date -Is)
- 复现命令：
  \`\`\`bash
  bash scripts/perf/c/s3-static.sh        # 端口 $PORT，分核 $PC_CPUS，自己 pc_build/pc_start/pc_stop
  # 手工单点（服务起着时）：
  taskset -c $PC_CPUS curl -sI "\$PC_BASE/tailwind.<hash>.css"          # 看有无 ETag/Last-Modified
  taskset -c $PC_CPUS curl -sI -H 'Accept-Encoding: gzip, br' "\$PC_BASE/tailwind.<hash>.css"
  \`\`\`
- 原始证据：\`$RAW/headers.txt\`（全量头 dump + 每个条件请求的结果）、\`$RAW/table.tsv\`、\`$RAW/*.hdr\`、
  \`$RAW/html-index.html\`、\`$RAW/fonts-css.body\`、\`$RAW/gzip-estimate.tsv\`、\`$RAW/firstscreen-bytes.tsv\`、\`$RAW/embedprobe/main.go\`（embed.FS modtime 探针）

---

## 1. 缓存分级表（实测，服务为 \`-tags embeddist\` 单二进制）

对象全部从 \`GET /\` 的 HTML / 主 JS / 字体 CSS 正文里**抽取**得到（不是硬编码文件名）。
「传输字节(plain)」是 \`curl -w %{size_download}\` 实测；「gzip,br」列是同一请求加
\`Accept-Encoding: gzip, br\` 后的实测 \`size_download\`。

纵向对比（C3 没有时间序列维度，这里的「对照」是**同一对象的两种测量口径**，都来自实测）：

| 对比维度 | A | B | 差异 |
|---|---|---|---|
| 缓存档位 | 指纹资源（$N_SUB_FIN 条）\`max-age=31536000, immutable\` | 非指纹资源 \`max-age=60\` | 一年免请求 vs 60 秒后必回源 |
| 压缩 | plain（\`%{size_download}\` 实测） | 带 \`Accept-Encoding: gzip, br\` 实测 | **完全一致** → 服务端没压 |
| 若压缩生效（本地 gzip -9 估算） | 47682 / 81819 / 48257 字节 | 14366 / 26192 / 12179 字节 | 3.32x / 3.12x / 3.96x |
| 条件请求 | 带真 ETag / 带真 Last-Modified | 不带 | 服务端连头都不发，\`If-*\` 一律 200 全量 |
| 重定向 | 不跟随（301，带 max-age=60） | 跟随（200，全量字节） | 两跳都无法 304 |

| 资源 | 路径 | status（不跟随→跟随） | Cache-Control | ETag | Last-Modified | If-None-Match 码 | If-Modified-Since 码 | Content-Encoding | 传输字节 plain → gzip,br |
|---|---|---|---|---|---|---|---|---|---|
MD
	cat "$MDT"
	cat <<MD

（\`无ETag\`/\`无LM\` = 响应里根本没有那个头，条件请求无从发起；\`—\` = 该头缺失。
key 含义：\`root\` 即 \`/\`；\`app_js\`/\`app_css\`=HTML 里最大的 js/css；\`font\`/\`image\`=字体/图标；
\`sw_js\`/\`manifest\`=\`/sw.js\` 与 webmanifest，\`dir_no_slash\`/\`en_dir\`=目录缺尾斜杠（301 样本），
\`not_found_*\`=不存在路径。\`If-None-Match\`/\`If-Modified-Since\` 两列都是在**最终跳**上发出去的。）

重定向链（任务要求「跟随重定向前后各测一次」）：**\`/index.html\` 确实 301**——\`Location: ./\`，
跟随之后才是 \`/\` 的 200（表里 status 列写成 \`301→200\` 就是这个意思）。关键点是：
**这个 301 本身也带 \`public, max-age=60\`，但同样既无 ETag 也无 Last-Modified**（\`classify("/index.html")\`
落 \`cacheEntry\` 分支），所以「\`/index.html\` → \`/\`」这条链每一跳都得走完整往返，没有一跳能 304。
\`/assets\`（301 → \`/assets/\` 目录列表 200）和 \`/en\`（301 → \`/en/index.html\` 200）同理。

---

## 2. ETag / 304：**完全没有**条件请求能力（embed 模式下 Last-Modified 也没有）

数字（\`$RAW/table.tsv\` 的 \`inm\`/\`ims\` 两列）：

- 全部 $N_OBJ 个对象里，\`ETag\` 命中 **0 个**；\`Last-Modified\` 命中 **0 个**。
- 拿一个不存在的 \`ETag\` 去试探（\`If-None-Match: "s3-probe-does-not-exist"\`）：一律是 **200 + 全量字节**，
  说明服务端连「比较一下」都没做，不是「比较了但没命中」。
- 拿「任何文件都比它旧」的 \`If-Modified-Since: Thu, 01 Jan 2099\` 试探：同样 **200 + 全量字节**（见
  \`headers.txt\` 每个对象的 \`[4b]\` 段）——服务端**没有可用的 modtime**，无从比较。
- 结论：**304 命中率 = 0%**。每个对象、每次请求都是完整 200 响应体。

初步归因（\`internal/server/static.go\`）：

| 事实 | 出处 |
|---|---|
| 静态处理器直接把请求交给 \`http.FileServer\`，只额外设 \`Cache-Control\` | \`internal/server/static.go:53-64\`（\`staticFS\`：\`:54\` \`fs := http.FileServer(fsys)\` → \`:56-61\` 按 \`classify()\` 设 \`Cache-Control\` → \`:62\` \`fs.ServeHTTP\`） |
| 全文件没有任何 \`ETag\` / \`Last-Modified\` / \`Content-Encoding\` 的写入 | \`grep -nE 'ETag\|Last-Modified\|Content-Encoding' internal/server/static.go\` → 只命中 \`Cache-Control\` 一处 |
| \`classify()\` 的三条分支：\`*.html\`/\`sw.js\`/\`manifest.json\` → \`cacheEntry\`；\`stem.<8hex>.<ext>\` → \`cacheImmutable\`；其余 → \`cacheEntry\` | \`internal/server/static.go:21-35\` |
| \`cacheEntry\` 只给 \`public, max-age=60\`，注释里写的「短缓存 + ETag」**并没有实现** | \`internal/server/static.go:16\`（\`cacheEntry // 入口 HTML / sw.js：短缓存 + ETag\`）vs \`:60\`（\`w.Header().Set("Cache-Control", "public, max-age=60")\`） |
| Go 标准库 \`http.FileServer\`/\`serveContent\` 本身也不产生 ETag（只有 \`Last-Modified\` + \`If-Modified-Since\`） | 标准库行为；\`serveContent\` 里只有 \`setLastModified\`，无 \`ETag\` |
| embed 模式下连 \`Last-Modified\` 都没有：\`embed.FS\` 的 \`FileInfo.ModTime()\` 是零值，而标准库 \`setLastModified\` 对零时间**不写头** | 实测：\`$RAW/embedprobe/main.go\` 的输出见 \`headers.txt\` 第 3 节；\`staticFS\` 的 fsys 来自 \`embeddedStatic()\`（\`internal/server/embed_dist.go\`），\`resolveStatic\` 在 \`StaticDir\` 为空时走 embed（\`internal/server/static.go:92-118\`） |

一句话：**「短缓存 + ETag」这个设计意图从未落地**；在 embed（生产默认）下连标准库自带的
\`Last-Modified\` 都因为零 modtime 被吞掉，于是 \`.html\`/\`sw.js\`/manifest 三者退化成
「60 秒后必须全量重下」。

附带事实（\`classify()\` 的判定与实际产物不符）：

- 产物里 manifest 叫 \`manifest.webmanifest\`（\`scripts/hash.mjs\` 的 \`NO_FINGERPRINT\` 列表），
  但 \`classify()\` 判的是 \`base == "manifest.json"\`（\`internal/server/static.go:23\`）。
  它最后仍然拿到 \`max-age=60\`，但走的是**函数末尾的兜底 return**，不是那条显式分支——
  一旦以后有人把兜底改成别的默认值，manifest 会静默跟着变。
- \`.webmanifest\` 在 \`scripts/hash.mjs\` 的 \`FINGERPRINT_EXT\` 里，但被 \`NO_FINGERPRINT\` 优先拦下，
  所以路径固定、不带指纹——这正是它需要 ETag/304 的原因。

---

## 3. gzip：**完全没有**压缩，大 CSS/JS 的传输放大倍数见下

- 对最大 CSS 加 \`Accept-Encoding: gzip, br\` → 响应里**没有** \`Content-Encoding\`（实测取值：${GZ_CE:-无}），\`size_download\` 与不带时**完全一致**（${CSS_PLAIN} 字节）——请求带了 gzip/br，服务端照样吐 plain。
- 对最大 JS 同样：\`Content-Encoding\` = ${JS_CE:-无}（无该头），${JS_PLAIN} 字节。
- 连 \`Vary: Accept-Encoding\` 都没有（没有压缩也就不需要，但这说明链路上确实不存在压缩段）。
- 本地 \`gzip -9\` 估算同内容压完的体积（\`raw/gzip-estimate.tsv\`，**估算值，不是服务端输出**）：

| 对象 | plain 字节 | gzip -9 字节 | 放大倍数（plain/gzip） |
|---|---|---|---|
| 最大 CSS | $CSS_PLAIN | $CSS_GZ9 | **${CSS_RATIO}** |
| 最大 JS | $JS_PLAIN | $JS_GZ9 | **${JS_RATIO}** |
| \`/\`（HTML 入口） | $ROOT_PLAIN | $ROOT_GZ9 | **${ROOT_RATIO}** |

归因：\`internal/server/mux.go\` 里**没有任何压缩中间件**。\`NewApp\` 用 \`http.NewServeMux()\` 装配，
\`mux.Handle("/", resolveStatic(cfg))\`（\`internal/server/mux.go:416\`）直接挂静态处理器，
动态 API 那条链路才套 \`noStore(...)\`（\`internal/server/static.go:121-126\`，它只设 \`Cache-Control\`），静态链路一个中间件都没套。
实证：\`grep -rnE 'gzip|deflate|brotli|Compress' internal/ cmd/ --include='*.go'\` → **零匹配**（输出在
\`headers.txt\` 第 3 节）。所以压缩只能靠前置 nginx/CDN；源站直连（本场景、以及 README 里的
单二进制部署）没有任何压缩。

字体（\`.woff2\`）本身已压过，gzip 收益接近 0——所以「缺 gzip」的伤害集中在文本资源（HTML/JS/CSS）上。

---

## 4. 影响判断

### 4.1 首屏每次导航要重新校验/重传多少东西

从 \`GET /\` 的 HTML 里数（\`raw/headers.txt\` 第 1 节）：同源引用共 **$N_ALL 条**，其中
**子资源 $N_SUB 条**（浏览器会自动请求）、**导航链接 $N_NAV 条**（只是 \`<a href>\`，点了才请求）。
子资源里**带 8 位指纹 $N_SUB_FIN 条**（走 \`immutable\`，一年免请求）、**不带指纹 $N_SUB_NOF 条**：

\`\`\`
$(sub_refs | grep -vE '\.[0-9a-f]{8}\.[A-Za-z0-9]+$' | sed 's/^/    /')
\`\`\`

带指纹的那些确实拿到了 \`public, max-age=31536000, immutable\`（表里 \`app_js\`/\`app_css\`/\`font\`/\`image\` 四行），
这部分设计是对的。**首屏一次性要拉 $FS_PLUS_DOC 字节 plain**（HTML + $N_SUB 个子资源，逐个
\`Content-Length\` 求和，见 \`raw/firstscreen-bytes.tsv\`），它们大多带指纹，冷启动一次之后不再付这个代价。

每次导航真正会重复付的是**不带指纹的那 $N_SUB_NOF 个 + HTML 文档本身**，而它们的
\`Cache-Control\` 是 \`max-age=60\`：60 秒内不回源，60 秒后**必然回源**；因为无 ETag 无
Last-Modified，回源就是**200 + 全量字节**：

| 对象 | Cache-Control | 60s 后回源的代价 |
|---|---|---|
| \`/{index,events,playbooks,...}.html\` | \`public, max-age=60\` | 200，$ROOT_PLAIN 字节（index 页；events $EV_PLAIN、playbooks $PB_PLAIN） |
| \`sw.js\` | \`public, max-age=60\` | 200，$SW_PLAIN 字节 |
| \`manifest.webmanifest\` | \`public, max-age=60\` | 200，$MAN_PLAIN 字节 |

即 **每次导航最多 3 个对象、约 $((ROOT_PLAIN + SW_PLAIN + MAN_PLAIN)) 字节**是「即使内容没变也要重传」的
（严格说 \`sw.js\` 是由浏览器按 Service Worker 规范做更新检查，不一定每次导航都发；
\`HTML\` 与 \`manifest\` 则是 60 秒后每次都必然重取）。
关键不是这 3 次请求的 RTT，而是 \`/\` 的 HTML 本身就有 $ROOT_PLAIN 字节、占首屏总量的大头，
而它恰好是最常被请求的那一个——分钟级频繁导航（反复刷新/切页）时这个重复量可以感知。

### 4.2 CDN/浏览器缓存会不会全部失效

**不会全失效**——指纹资源（JS/CSS/字体/图片）是 \`immutable\`，这一半是健康的；
失效的是叠加在它们上面的那一层：

- **入口 HTML 只有 \`max-age=60\`，且无 ETag**：60 秒窗口内任何一次部署，用户最多等 1 分钟就能拿到新
  HTML，这个数字本身不算离谱；但它同时是「HTML 引用新指纹 → 静态资源 URL 变 → 新 URL 未缓存」的
  唯一入口。缺 ETag 意味着**即使 HTML 内容完全没变**（绝大多数导航都是这种情况），60 秒后也拿不到
  304，只能整份重下 $ROOT_PLAIN 字节。ETag 的收益恰恰是「不变的时候几乎不传」，这一条现在是零。
- **\`/agent-login.sh\`：\`Cache-Control: no-store\`（最终 status=${ALSH_ST%%→*}）**——这条**不是问题**：
  \`internal/server/cli_auth_handlers.go:321\` 显式设了 \`no-store\`，安装脚本「永远取最新」是合理选择。
  写在这里只是把「静态路径下每条都拿到了缓存头」说全（它没有 ETag/Last-Modified，但 \`no-store\` 下用不上）。
- **404 不带缓存头**（\`raw/headers.txt\` 的 \`not_found_*\` 两节）：
  \`/s3-no-such-file.txt\` → status=${NF404TXT_ST%%→*}，\`Cache-Control: ${NF404TXT_CC:-（无）}\`；
  \`/ghost.0123abcd.js\`（**长得像指纹文件名**）→ status=${NF404_ST%%→*}，\`Cache-Control: ${NF404_CC:-（无）}\`。
  这与「\`classify()\` 在 \`fs.ServeHTTP\` 之前就把头写死，所以 404 也会带 immutable」的直觉相反——
  原因是 Go 1.23+ 的 \`fs.serveError\` 会把 \`Cache-Control\`/\`Content-Encoding\`/\`Etag\`/\`Last-Modified\`
  **从错误响应上删掉**（实测工具链 ${GOROOT_ACT}/src/net/http/fs.go:188-208；
  \`GODEBUG=httpservecontentkeepheaders=1\` 可恢复 Go≤1.22 的老行为）。
  **这一条是工具链依赖的**：本仓库 \`go.mod\` 钉 \`go 1.25.0\`、\`Dockerfile:8,27\` 用 \`golang:1.25-alpine\`，
  所以现在安全；但若用 Go ≤1.22 构建，\`/ghost.<hash>.js\` 这类 404 会**继承
  \`public, max-age=31536000, immutable\`**——一次部署失败/回滚期间的短暂 404 会被 CDN/浏览器按「一年」
  缓存，修好之后同名资源**仍然永远 404**，只能靠换指纹绕过。建议不依赖标准库这段兜底：
  在 \`staticFS\` 里自行判断存在性后再写 \`Cache-Control\`。

顺带测到的（不作为本场景主结论）：不带斜杠的目录 \`/assets\` → 301 → \`/assets/\`，
**且这个 301 也带 \`public, max-age=60\`**（\`classify("/assets")\` 落兜底分支）；\`/assets/\` 本身是
\`http.FileServer\` 的目录列表（200 + autoindex HTML，见 \`raw/headers.txt\` 的 \`dir_no_slash\` 节）。
\`/en\` → 301 → \`/en/\` → \`/en/index.html\`（200，$EN_PLAIN 字节）。也就是说 \`http.FileServer\` 的重定向
行为确实被 \`classify()\` 的缓存头覆盖到了，但没有它自己的 \`ETag\`/\`Last-Modified\`。

### 4.3 无 gzip 的传输放大倍数

用实测字节算：最大 CSS **${CSS_RATIO}**（$CSS_PLAIN → $CSS_GZ9 字节），最大 JS **${JS_RATIO}**
（$JS_PLAIN → $JS_GZ9 字节），HTML 入口 **${ROOT_RATIO}**（$ROOT_PLAIN → $ROOT_GZ9 字节）。
首屏整体（HTML + $N_SUB 个子资源，逐个 \`Content-Length\` 求和）**$FS_PLUS_DOC → 约 $((FS_GZ + ROOT_GZ9)) 字节**，
即 **约 $(awk -v a="$FS_PLUS_DOC" -v b="$((FS_GZ + ROOT_GZ9))" 'BEGIN{printf "%.1f", a/b}')x**
（\`raw/firstscreen-bytes.tsv\`；gzip 侧是本地 \`gzip -9\` 估算，\`.woff2\` 本身已压过所以拉低了整体倍数）。
这些都在**首屏阻塞路径**上（HTML → CSS/JS），所以放大倍数直接乘在首屏关键路径上；
换成 brotli 还能再省一截（未测）。

### 4.4 增长关系

这是**一次性、与数据量无关**的常量税：不随时间线性增长，也不会累积成泄漏。
它降低的是每一次冷/半冷导航的体验下限（首屏 $FS_PLUS_DOC 字节 → 有 gzip 时约 $((FS_GZ + ROOT_GZ9)) 字节），
外加每 60 秒一次、每次最多 3 个对象的全量重传。属于「一直存在但不恶化」的类型——
和 C1/C2 那种随时间/数据量单调增长的问题性质不同（契约 §5 要区分的就是这两类）。

### 4.5 修复方向（只给建议，本场景不改代码）

1. \`staticFS\` 里按内容哈希算 \`ETag\`（embed 下可以直接用 \`hash8(内容)\` 或复用指纹名），
   并在 \`fs.ServeHTTP\`（\`static.go:62\`）**之前**处理 \`If-None-Match\` → 304；这同时解决 embed 无
   \`Last-Modified\` 的问题（指纹名本身就是 \`immutable\`，只有 \`cacheEntry\` 类需要 ETag）。
2. 补压缩（**所有文本类都要**：\`cacheEntry\` 的 HTML 与 \`cacheImmutable\` 的 JS/CSS 一样吃亏——
   指纹只免除「第二次」的传输，首屏那 402534 字节 JS/CSS 照样是 plain 传的）：
   在 \`staticFS\` 里用 \`compress/gzip\`（或 brotli）+ \`Vary: Accept-Encoding\`，
   或明确文档化「必须在反代开 gzip」并加一条部署自检（否则单二进制直连部署永远是 plain）。
3. \`classify()\` 把 \`.webmanifest\` 也显式认下来（现在只是碰巧落兜底）；
   404/301 不要依赖 \`fs.serveError\` 删头（Go 1.23+ 才有、\`GODEBUG\` 可关），
   自己在 \`staticFS\` 里先判存在性再写 \`Cache-Control\`；目录请求（\`/assets\`）按需关掉 autoindex。
MD
} >"$OUT/finding.md"

hr "完成"
log "finding : $OUT/finding.md"
log "raw     : $RAW/headers.txt ($(wc -l <"$HDR") 行)"
log "table   : $TAB"
printf 's3: finding.md → %s\n' "$OUT/finding.md"
printf 's3: raw/headers.txt %s 行\n' "$(wc -l <"$HDR")"
