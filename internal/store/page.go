package store

// 列表 keyset 分页的内部口径（#377）。
//
// 八个列表（sources / capabilities / playbooks / nodes / keys / admin users /
// auth sessions / auth passkeys）共用同一套语义：
//
//   - `limit` 缺省 50（HTTP 契约的 50/500 夹取在 internal/server 的 pageLimit）；
//   - 游标是 **keyset**（排序键 + 破平局的 id），不是 OFFSET：翻页只比较排序键，
//     所以跨页插入/删除不会让同一行出现两次或被跳过；`after*` 全零值 = 第一页；
//   - 「还有下一页」由调用方多取一行判定，store 不猜。
//
// 这里只做兜底夹取。上限是 pageFetchCap（501）而不是 500：handler 用 `limit+1`
// 多取一行来判定 `continue`，把多取的那行也夹回 500 会让「恰好 500 条一页」永远
// 报成末页。业务上限（500）在 handler 侧，store 不重复表达。
const (
	pageDefaultLimit = 50
	pageFetchCap     = 501
)

// normalizePageLimit 是分页查询的兜底夹取：<=0 → 50，>501 → 501。
func normalizePageLimit(limit int) int {
	if limit <= 0 {
		return pageDefaultLimit
	}
	if limit > pageFetchCap {
		return pageFetchCap
	}
	return limit
}
