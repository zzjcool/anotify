package store

import (
	"context"
	"fmt"
	"strings"
)

// DependencyConsumer 是一条「这个能力被已生效的安装依赖」的事实（#534 依赖锁的判定单元）。
// 身份是 (playbook, release)：同一个 release 装过几次都只算一个消费者。
type DependencyConsumer struct {
	Playbook string `json:"playbook"`
	Release  string `json:"release"`
}

// CapabilityLockedError 是能力删除被依赖锁拒绝的理由。
// 返回它时整事务已回滚（0 写）：能力一行没动，删除是「不可以」而不是「删了一半」。
type CapabilityLockedError struct {
	Capability string
	Consumers  []DependencyConsumer
}

func (e *CapabilityLockedError) Error() string {
	names := make([]string, 0, len(e.Consumers))
	for _, c := range e.Consumers {
		names = append(names, fmt.Sprintf("%s/%s", c.Playbook, c.Release))
	}
	return fmt.Sprintf("能力 %q 被已生效的预案安装依赖（%s）：先卸载这些 release 再删它",
		e.Capability, strings.Join(names, ", "))
}

// capabilityConsumersOn 列出依赖该能力的**已生效**安装（uninstalled_at = 0），按 (slug, release) 去重。
//
// 去重放在 SQL 侧（GROUP BY）：同一个 (slug, release) 可以有多次安装回执，各自写了自己的依赖行；
// 锁判定要回答的是「哪些实例还在用它」，不是「哪些回执提过它」。
func capabilityConsumersOn(ctx context.Context, ex queryExec, namespaceID, name string) ([]DependencyConsumer, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT pi.playbook_slug, pi.release
		   FROM playbook_install_requires r
		   JOIN playbook_installs pi ON pi.id = r.install_id
		  WHERE r.namespace_id = ? AND r.capability = ? AND pi.uninstalled_at = 0
		  GROUP BY pi.playbook_slug, pi.release
		  ORDER BY pi.playbook_slug, pi.release`, namespaceID, name)
	if err != nil {
		return nil, fmt.Errorf("capability %q consumers: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	out := []DependencyConsumer{}
	for rows.Next() {
		var c DependencyConsumer
		if err := rows.Scan(&c.Playbook, &c.Release); err != nil {
			return nil, fmt.Errorf("scan capability %q consumer: %w", name, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate capability %q consumers: %w", name, err)
	}
	return out, nil
}
