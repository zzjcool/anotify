package store

// config_seed_test.go 覆盖 #005 引入的 SeedSystemConfigIfEmpty：
// env `ANOTIFY_EVENT_RETENTION_DAYS` 从「只进启动日志」变成「空库首次启动播种 system_config」，
// 而 system_config 仍然是**唯一**运行期来源（RunRetention 每轮 GetSystemConfig）。
//
// 四条口径（缺一条就会退化成双源或空转）：
//  1. 空库 → 真的写进去，读回逐字段一致，resource_version=1；
//  2. 再调一次 → false 且值不变（幂等，重启不重写）；
//  3. 运维 PUT 过 → seed **不动它**（env 不覆盖运行期配置）；
//  4. nil / 负数 → 报 ErrConfigInvalid 且零写入（store 不接受负数，「0=永久」的归一在解析层）。

import (
	"context"
	"errors"
	"testing"
)

func TestSeedSystemConfigIfEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("空库播种后读回一致且 rv=1", func(t *testing.T) {
		db := newTestDB(t)
		seed := &SystemConfig{
			RetentionEventTTLSec:  90 * 86400,
			RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		}
		ok, err := db.SeedSystemConfigIfEmpty(ctx, seed)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if !ok {
			t.Fatal("空库必须播种成功（返回 true）")
		}

		// 往返一致性：存进去什么，读出来就是什么（本包 §3.3 的硬要求）。
		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RetentionEventTTLSec != seed.RetentionEventTTLSec ||
			got.RetentionTaskTTLSec != seed.RetentionTaskTTLSec ||
			got.RetentionResultTTLSec != seed.RetentionResultTTLSec {
			t.Fatalf("播种值未原样落库：got %+v want %+v", got, seed)
		}
		// rv 必须是 1：与「无行」时报出的默认令牌一致，首次 PUT 的前置条件才不会被打乱，
		// 「rv>1」也才能继续当「运维 PUT 过」的证据（server 侧 source 判定依赖它）。
		if got.ResourceVersion != 1 {
			t.Fatalf("播种行的 resourceVersion 必须是 1，实际 %d", got.ResourceVersion)
		}

		// 播种后首次 PUT 仍然可用（rv=1 是合法前置条件）——这条钉住「播种不会把库写成不可写」。
		if err := db.PutSystemConfig(ctx, &SystemConfig{
			RetentionEventTTLSec:  7 * 86400,
			RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		}, 1); err != nil {
			t.Fatalf("播种后首次 PUT 必须成功（rv=1）：%v", err)
		}
	})

	t.Run("幂等：第二次返回 false 且值不变", func(t *testing.T) {
		db := newTestDB(t)
		first := &SystemConfig{RetentionEventTTLSec: 90 * 86400, RetentionTaskTTLSec: 100, RetentionResultTTLSec: 200}
		if ok, err := db.SeedSystemConfigIfEmpty(ctx, first); err != nil || !ok {
			t.Fatalf("首次播种应成功：ok=%v err=%v", ok, err)
		}
		// 第二次用一个**不同的值**：如果实现是 upsert，这里会被改写。
		second := &SystemConfig{RetentionEventTTLSec: 1, RetentionTaskTTLSec: 1, RetentionResultTTLSec: 1}
		ok, err := db.SeedSystemConfigIfEmpty(ctx, second)
		if err != nil {
			t.Fatalf("第二次播种报错（应静默返回 false）：%v", err)
		}
		if ok {
			t.Fatal("已有行时第二次播种必须返回 false")
		}
		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RetentionEventTTLSec != first.RetentionEventTTLSec ||
			got.RetentionTaskTTLSec != first.RetentionTaskTTLSec ||
			got.RetentionResultTTLSec != first.RetentionResultTTLSec {
			t.Fatalf("已有行被覆盖了：got %+v want %+v", got, first)
		}
	})

	t.Run("运维 PUT 过之后 env 不覆盖", func(t *testing.T) {
		db := newTestDB(t)
		// 模拟「运维在控制台把保留期改成 7 天」，随后进程重启（env 里还是旧的 90 天）。
		if err := db.PutSystemConfig(ctx, &SystemConfig{
			RetentionEventTTLSec:  7 * 86400,
			RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		}, 1); err != nil {
			t.Fatalf("put: %v", err)
		}
		ok, err := db.SeedSystemConfigIfEmpty(ctx, &SystemConfig{
			RetentionEventTTLSec:  90 * 86400,
			RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if ok {
			t.Fatal("已有行（PUT 过）时播种必须返回 false")
		}
		got, err := db.GetSystemConfig(ctx)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.RetentionEventTTLSec != 7*86400 {
			t.Fatalf("运行期配置被 env 覆盖了：events TTL = %d，want %d", got.RetentionEventTTLSec, 7*86400)
		}
		if got.ResourceVersion != 2 {
			t.Fatalf("PUT 之后 resourceVersion 应是 2，实际 %d", got.ResourceVersion)
		}
	})

	t.Run("nil 与负数报错且零写入", func(t *testing.T) {
		db := newTestDB(t)
		if ok, err := db.SeedSystemConfigIfEmpty(ctx, nil); err == nil || ok {
			t.Fatalf("nil 必须报错且不播种：ok=%v err=%v", ok, err)
		}
		// 只把**一个**字段写成负数：错误文案按 map 迭代顺序取名，多条同时为负时
		// 「哪一条先报」不确定（PutSystemConfig 也是同一写法），测试不要依赖那个顺序。
		ok, err := db.SeedSystemConfigIfEmpty(ctx, &SystemConfig{RetentionEventTTLSec: -1})
		if !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("负数应报 ErrConfigInvalid，实际 err=%v", err)
		}
		if ok {
			t.Fatal("负数不得播种")
		}
		// 零写入的证据：表里仍然没有行 —— 再播种一次必须成功（= 之前没留下半行）。
		if ok, err := db.SeedSystemConfigIfEmpty(ctx, &SystemConfig{
			RetentionEventTTLSec:  30 * 86400,
			RetentionTaskTTLSec:   DefaultRetentionTaskTTLSec,
			RetentionResultTTLSec: DefaultRetentionResultTTLSec,
		}); err != nil || !ok {
			t.Fatalf("非法输入之后库必须仍是空表：ok=%v err=%v", ok, err)
		}
	})
}
