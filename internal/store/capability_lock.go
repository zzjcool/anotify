package store

import (
	"context"
	"fmt"
	"strings"
)

type DependencyConsumer struct {
	Playbook string `json:"playbook"`
	Release  string `json:"release"`
}

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

// capabilityConsumersOn 只统计**清单仍在**的安装：清单删了那条回执既看不见也卸不掉（卸载按 playbook_id 找回执）。
func capabilityConsumersOn(ctx context.Context, ex queryExec, namespaceID, name string) ([]DependencyConsumer, error) {
	rows, err := ex.queryContext(ctx,
		`SELECT pi.playbook_slug, pi.release
		   FROM playbook_install_requires r
		   JOIN playbook_installs pi ON pi.id = r.install_id
		   JOIN playbooks pb ON pb.id = pi.playbook_id AND pb.namespace_id = r.namespace_id
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
