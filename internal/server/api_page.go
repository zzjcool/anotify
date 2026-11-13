package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 列表分页的 HTTP 口径（#377）。
//
// 八个列表端点（sources / capabilities / playbooks / nodes / keys / admin users /
// auth sessions / auth passkeys）共用同一套语义，与 `console/events`、
// `tasks/{id}/emissions` 逐字一致：
//
//   - `limit`：缺省 50；非整数 → 400（`limit must be an integer`）；数值越界不报错：
//     `<=0` 按 50 处理，`>500` 钳到 500（不悄悄换成更小的窗口——#236 的回归）；
//   - `continue`：不透明游标，绑定查询作用域（namespace / 用户 / 列表身份）。坏令牌、
//     跨作用域的令牌 → 400；**不**做「尽力解释」——静默回退成第一页会让翻页莫名
//     跳回最新，跨作用域更会变成读越权记录；
//   - 末页不带 `continue`（资源信封的 `metadata.continue` 省略；非信封列表回 `""`）。
//
// 游标是 **keyset**：令牌里存上一页最后一行的排序键（排序键 + 破平局的 id），
// 翻页只做 `<`/`>` 比较，不用 OFFSET——跨页插入/删除不会让同一行出现两次或被跳过。
const (
	pageDefaultLimit = 50
	pageLimitCap     = 500
)

// pageLimit 解析 `?limit=`（口径见上）。
func pageLimit(r *http.Request) (int, error) {
	n, err := taskQueryInt(r, "limit", 0)
	if err != nil {
		return 0, err
	}
	return taskClampListLimit(n), nil
}

// pageLimitPlain 是 auth / admin 域（错误体是 `{error}` 而不是 Status 信封）的
// 同一套 limit 口径；第二个返回值非空表示 400 文本。
func pageLimitPlain(r *http.Request) (int, string) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return pageDefaultLimit, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, "limit must be an integer"
	}
	if n <= 0 {
		return pageDefaultLimit, ""
	}
	if n > pageLimitCap {
		return pageLimitCap, ""
	}
	return n, ""
}

// pageCursorEnvelope 是通用 keyset 游标：binding + 排序键的字符串投影。
//
// 与 `cursorEnvelope`（events / emissions 的 (created_at, id)）并存而不是替换：
// 那些端点已经在用 `{b,i,t}`，换形状会让已经发出去的游标失效。这里只是把「位置」
// 从两个固定字段放宽成有序键列表——各列表的排序键个数与类型本来就不同
// （capabilities 是 (name, version)，其余是 (created_at|name, id)）。
// 令牌里没有秘密，因此不签名：防的是误用与越权组合，不是伪造。
type pageCursorEnvelope struct {
	B string   `json:"b"`
	K []string `json:"k"`
}

// encodePageCursor 生成不透明 continue 令牌（keys 按列表的排序键顺序）。
func encodePageCursor(binding string, keys ...string) string {
	raw, err := json.Marshal(pageCursorEnvelope{B: binding, K: keys})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodePageCursor 解码并校验 binding 与键个数；want 是列表的排序键个数。
func decodePageCursor(raw, binding string, want int) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errPageCursorMalformed()
	}
	var env pageCursorEnvelope
	if err := json.Unmarshal(decoded, &env); err != nil {
		return nil, errPageCursorMalformed()
	}
	if env.B != binding {
		return nil, errStatus(http.StatusBadRequest, reasonBadRequest,
			"continue token does not belong to this query")
	}
	if len(env.K) != want {
		return nil, errPageCursorMalformed()
	}
	for _, key := range env.K {
		if key == "" {
			return nil, errPageCursorMalformed()
		}
	}
	return env.K, nil
}

// errPageCursorMalformed 是与既有游标解码同族的 400（不新增错误码）。
func errPageCursorMalformed() error {
	return errStatus(http.StatusBadRequest, reasonBadRequest, "continue token is malformed")
}

// pageCursorInt64 把游标里的整数排序键解回来；坏值按坏令牌处理。
func pageCursorInt64(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errPageCursorMalformed()
	}
	return n, nil
}

// pageCursorMessage 把游标错误翻成 auth / admin 域的错误文本
// （同一批 400，只是错误体形状不同：那边是 `{error}`，不是 Status 信封）。
func pageCursorMessage(err error) string {
	var ae *apiError
	if asAPIError(err, &ae) {
		return ae.Message
	}
	return err.Error()
}

// pageKeyInt64 是整数排序键的字符串投影（与 pageCursorInt64 互逆）。
func pageKeyInt64(v int64) string { return strconv.FormatInt(v, 10) }

// pageAfterTimeID 解析 `(created_at, id)` 两键游标；没有游标时返回 (0, "", nil)。
func pageAfterTimeID(r *http.Request, binding string) (int64, string, error) {
	keys, err := decodePageCursor(r.URL.Query().Get("continue"), binding, 2)
	if err != nil || len(keys) == 0 {
		return 0, "", err
	}
	at, err := pageCursorInt64(keys[0])
	if err != nil {
		return 0, "", err
	}
	return at, keys[1], nil
}

// pageAfterNameID 解析 `(name, id)` 两键游标（nodes）；没有游标时返回 ("", "", nil)。
func pageAfterNameID(r *http.Request, binding string) (string, string, error) {
	keys, err := decodePageCursor(r.URL.Query().Get("continue"), binding, 2)
	if err != nil || len(keys) == 0 {
		return "", "", err
	}
	return keys[0], keys[1], nil
}

// pageCut 收口「多取一行判定还有没有下一页」+ 生成末行游标。
//
// rows 是 `limit+1` 条候选；返回本页切片（最多 limit 条）与 continue 令牌
// （末页为空串）。keyOf 把一行投影成它的排序键（与列表的 ORDER BY 同序）。
func pageCut[T any](rows []T, limit int, binding string, keyOf func(T) []string) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	if len(rows) == 0 {
		return rows, ""
	}
	return rows, encodePageCursor(binding, keyOf(rows[len(rows)-1])...)
}
