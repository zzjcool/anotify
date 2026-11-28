package store

// #617：这四个纯读入口不许再包 `BeginTx(ctx, nil)` —— `_txlock=immediate` 下空事务也抢写锁，
// 写者持锁时它们在 busy_timeout(5000ms) 后报 database is locked（实测 4.9s），而它们在握手 / 派发 /
// GET /v1/nodes 热路径上。判据 = WAL 的「写不挡读」：写者持锁期间须毫秒级返回，去掉修复必红。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// blockedReadDeadline 故意小于 busy_timeout(5s)：回归时快速报错，而不是四条读各等 5s。
const blockedReadDeadline = 2 * time.Second

// blockedReadBudget=1s：抓的是 busy_timeout 量级的排队，比 issue 举例的 100ms 宽以抗抖动。
const blockedReadBudget = time.Second

func TestReadPaths_NotBlockedByWriter(t *testing.T) {
	t.Setenv(maxConnsEnv, "") // 钉住默认池：池=1 时读者拿不到自己的连接，红的原因会变味
	db, _ := newTestDBFile(t)
	ctx := context.Background()

	// 已知 slug 的命名空间：让授权列表是具体值，这样断言能区分「读到真行」与「读到缺省」。
	ns, err := db.CreateNamespace(ctx, "prod", "prod")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	node := &Node{Name: "read-paths", Status: NodeStatusOnline, DeclaredNamespaces: []string{NamespaceWildcard}}
	if _, err := db.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if node, err = db.GetNodeByName(ctx, "read-paths"); err != nil {
		t.Fatalf("get node: %v", err)
	}

	// 三层 vars 都填上值：local 层只有 hello 会写，所以本机快照走 RecordHello，
	// 中枢侧默认值走 PutNodeConfig（两条都是产品写入路径，不是直接改库）。
	if err := db.RecordHello(ctx, node.ID, NodeFacts{Hostname: "h1"}, nil, []string{NamespaceWildcard},
		map[string]string{"FOO": "local"}, map[string]any{"total": 4}, "", ""); err != nil {
		t.Fatalf("record hello: %v", err)
	}
	cfg, err := db.GetNodeConfig(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node config: %v", err)
	}
	cfg.Namespaces = []string{"prod"}
	cfg.Vars = map[string]string{"BAR": "node"}
	cfg.Limits = map[string]any{"total": 2}
	if err := db.PutNodeConfig(ctx, cfg); err != nil {
		t.Fatalf("put node config: %v", err)
	}
	if _, err := db.PutNamespaceConfig(ctx, ns.ID, map[string]string{"BAZ": "ns"}, ns.ResourceVersion); err != nil {
		t.Fatalf("put namespace config: %v", err)
	}

	// 真写者持锁（lease 巡检 / retention 批次 / 事件接收的形状）：no-op UPDATE 只占写锁、不碰
	// 被读的列 —— 失败只可能是锁等待，不可能是读到了写者的中间态。
	hold, err := db.BeginTx(ctx, nil) // DSN `_txlock=immediate` → 这里就是 BEGIN IMMEDIATE
	if err != nil {
		t.Fatalf("begin writer tx: %v", err)
	}
	if _, err := hold.ExecContext(ctx,
		`UPDATE nodes SET last_seen_at = last_seen_at WHERE id = ?`, node.ID); err != nil {
		_ = hold.Rollback()
		t.Fatalf("writer no-op update: %v", err)
	}

	reads := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"GetNodeConfig", func(c context.Context) error {
			cfg, err := db.GetNodeConfig(c, node.ID)
			if err != nil {
				return err
			}
			if cfg.Vars["BAR"] != "node" || cfg.LocalVars["FOO"] != "local" ||
				len(cfg.Namespaces) != 1 || cfg.Namespaces[0] != "prod" {
				return fmt.Errorf("读到的配置不对: %+v", cfg)
			}
			return nil
		}},
		{"AuthorizedNamespaces", func(c context.Context) error {
			got, err := db.AuthorizedNamespaces(c, node.ID)
			if err != nil {
				return err
			}
			if len(got) != 1 || got[0] != "prod" { // 授权 ["prod"] ∩ 申报 ["*"] = ["prod"]
				return fmt.Errorf("生效命名空间不对: %+v", got)
			}
			return nil
		}},
		{"EffectiveLimits", func(c context.Context) error {
			got, err := db.EffectiveLimits(c, node.ID)
			if err != nil {
				return err
			}
			if fmt.Sprint(got["total"]) != "4" { // local(4) 覆盖 node(2)
				return fmt.Errorf("生效 total 不对: %v", got["total"])
			}
			return nil
		}},
		{"EffectiveVars", func(c context.Context) error {
			got, err := db.EffectiveVars(c, node.ID, ns.ID, []string{"FOO", "BAR", "BAZ"})
			if err != nil {
				return err
			}
			for name, want := range map[string]string{
				"FOO": VarSourceLocal, "BAR": VarSourceNode, "BAZ": VarSourceNamespace,
			} {
				if got[name].Source != want {
					return fmt.Errorf("vars.%s 的层不对: %+v（想要 %s）", name, got[name], want)
				}
			}
			return nil
		}},
	}

	for _, r := range reads {
		readCtx, cancel := context.WithTimeout(ctx, blockedReadDeadline)
		start := time.Now()
		err := r.fn(readCtx)
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			t.Errorf("%s 在写者持锁期间必须照常读到（纯读不该抢写锁）：%v（耗时 %v）",
				r.name, err, elapsed)
			continue
		}
		if elapsed > blockedReadBudget {
			t.Errorf("%s 在写者持锁期间耗时 %v，超过 %v（说明它还在抢写锁，只是没等满 busy_timeout）",
				r.name, elapsed, blockedReadBudget)
			continue
		}
		t.Logf("%-22s while writer holds lock: %v", r.name, elapsed)
	}

	if err := hold.Rollback(); err != nil {
		t.Fatalf("rollback writer tx: %v", err)
	}

	// 语义回归：去掉事务不许把错误形状改软 —— 节点不存在仍然是具名 ErrNotFound。
	if _, err := db.GetNodeConfig(ctx, "nd_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetNodeConfig(不存在的节点) 应返回 ErrNotFound，实际: %v", err)
	}
	if _, err := db.AuthorizedNamespaces(ctx, "nd_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AuthorizedNamespaces(不存在的节点) 应返回 ErrNotFound，实际: %v", err)
	}
	if _, err := db.EffectiveLimits(ctx, "nd_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("EffectiveLimits(不存在的节点) 应返回 ErrNotFound，实际: %v", err)
	}
	if _, err := db.EffectiveVars(ctx, "nd_missing", ns.ID, []string{"FOO"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("EffectiveVars(不存在的节点) 应返回 ErrNotFound，实际: %v", err)
	}
}
