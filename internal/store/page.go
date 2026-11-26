package store

// 列表 keyset 分页的内部口径（#377）：八个列表（sources / capabilities / playbooks / nodes / keys /
// admin users / auth sessions / auth passkeys）共用同一套语义 —— `limit` 缺省 50（50/500 的夹取在
// internal/server 的 pageLimit）；游标是 **keyset**（排序键 + 破平局键，多数是 id、admin users 是
// rowid/插入序）而不是 OFFSET，所以跨页插入/删除不会让同一行出现两次或被跳过；`after*` 全零 = 第一页；
// 「还有下一页」由调用方多取一行判定。这里只做兜底夹取：上限 pageFetchCap（501）而不是 500 ——
// handler 用 `limit+1` 判 `continue`，夹回 500 会让「恰好 500 条一页」永远报成末页。
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
