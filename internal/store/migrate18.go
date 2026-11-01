package store

import (
	"database/sql"
	"fmt"
)

// migrateCapabilityToDeploy 是迁移 18（deployment_model）的 Go 逻辑部分：
// 把 node_capabilities 重建成「只从 deployments 派生」的形状，并摘掉 run_spec 列。
//
// # 为什么必须重建表
//
// SQLite 没有 DROP COLUMN。而 run_spec 这一列承载的是旧模型的语义
// （「节点上报它打算怎么跑」），新模型下执行方式属于产物、归属属于部署，
// 这一列既没有写者也没有读者 —— 留着它只会：
//   - 让「中枢手上有没有命令」这件事继续说不清（契约 I1 改写的前提是这一列消失）；
//   - 给下一个人「反正这里已经存过命令了」的滑坡理由。
//
// # 幂等
//
// 判据是「run_spec 列还在不在」：不在 = 已迁移，直接返回。
//
// # 为什么不用 stage 表
//
// 没有任何表的外键指向 node_capabilities（它只有出边），所以不需要迁移 12/14
// 那套 stage→drop→restore 三段式：直接新建、回灌、删旧、改名即可。
// 数据量也是全表最小的一张。
func migrateCapabilityToDeploy(tx *sql.Tx) error {
	has, err := hasColumn(tx, "node_capabilities", "run_spec")
	if err != nil {
		return fmt.Errorf("check node_capabilities.run_spec: %w", err)
	}
	if !has {
		return nil // 已迁移
	}

	// 重建期间外键检查延到提交：删旧表时会做一次隐式 DELETE。
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = TRUE`); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}

	stmts := []string{
		`DROP TABLE IF EXISTS node_capabilities_new`,
		`CREATE TABLE node_capabilities_new (
			node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			capability_id TEXT NOT NULL REFERENCES capability_versions(id) ON DELETE CASCADE,
			namespace_id  TEXT NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
			PRIMARY KEY (node_id, capability_id)
		)`,
		// 回灌时按 (node_id, capability_id) 去重：旧表主键本就是这两列，
		// 所以这里只防「历史上被手工改坏」的库。
		`INSERT OR IGNORE INTO node_capabilities_new (node_id, capability_id, namespace_id)
		   SELECT node_id, capability_id, namespace_id FROM node_capabilities`,
		`DROP TABLE node_capabilities`,
		`ALTER TABLE node_capabilities_new RENAME TO node_capabilities`,
		`CREATE INDEX IF NOT EXISTS idx_nodecap_cap ON node_capabilities(capability_id)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("rebuild node_capabilities: %w", err)
		}
	}
	return nil
}
