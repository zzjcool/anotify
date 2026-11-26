package store

import (
	"time"

	"github.com/zzjcool/anotify/internal/protocol"
)

// SetLeaseTTL 设置本库发放与续租租约使用的时长（中枢启动时从配置注入一次）。
//
// 为什么租约时长在 store 而不是调用方：`lease_until` 是**这一行的事实**，
// grant/renew 两个写入点必须用同一个数，否则节点按 A 续租、中枢按 B 判过期，
// 症状是「任务莫名其妙被判失联」。<=0 表示未配置 → 回落协议下限 60s。
func (d *DB) SetLeaseTTL(ttl time.Duration) {
	if ttl <= 0 {
		d.leaseTTL.Store(0)
		return
	}
	d.leaseTTL.Store(int64(ttl))
}

// LeaseTTL 返回生效的租约时长（未配置 → 协议下限）。
func (d *DB) LeaseTTL() time.Duration {
	if v := d.leaseTTL.Load(); v > 0 {
		return time.Duration(v)
	}
	return protocol.LeaseTTL
}

// leaseUntil 返回 now + 生效租约时长（秒口径，与 lease_until 列一致）。
//
// **向上取整**：lease_until 存的是秒，向下截断会让实际租约比配置短最多 1s——
// 180s 的生产默认只是白扔一截抖动余量，但 TTL 越短越致命（1s 时会短一半，
// 续租周期内的抖动直接判死）。租约宁长 1s 不短 1s。
func (d *DB) leaseUntil(now int64) int64 {
	ttl := d.LeaseTTL()
	secs := int64((ttl + time.Second - 1) / time.Second)
	return now + secs
}
