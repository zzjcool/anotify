package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzjcool/anotify/internal/sourcespec"
)

func requireCASRow(result sql.Result, uid string, revision int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resource CAS rows: %w", err)
	}
	if n != 1 {
		return resourceVersionConflict(uid, revision)
	}
	return nil
}

// CapabilityPatch covers the non-content exceptions. Nil means omitted; empty clears.
type CapabilityPatch struct {
	DisplayName *string
	Next        *[]string
}

// UpdateCapability checks live namespace/name/UID/revision and changes the row and projection atomically.
func (d *DB) UpdateCapability(ctx context.Context, namespaceID, name string, version int, patch CapabilityPatch, expectedVersion string) (*CapabilityVersion, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update capability: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	var cur *CapabilityVersion
	if version > 0 {
		cur, err = getCapabilityOn(ctx, ex, namespaceID, name, version)
	} else {
		cur, err = getCapabilityLatestOn(ctx, ex, namespaceID, name)
	}
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, cur.ID, cur.ResourceVersion); err != nil {
		return nil, err
	}
	updated, err := updateCapabilityOn(ctx, ex, cur, patch)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update capability: commit: %w", err)
	}
	return updated, nil
}

func updateCapabilityOn(ctx context.Context, ex queryExec, cur *CapabilityVersion, patch CapabilityPatch) (*CapabilityVersion, error) {
	updated := *cur
	if patch.DisplayName != nil {
		updated.DisplayName = *patch.DisplayName
	}
	if patch.Next != nil {
		updated.Spec.Next = append([]string{}, (*patch.Next)...)
	}
	norm, err := normalizeCapability(&updated)
	if err != nil {
		return nil, err
	}
	raw, err := marshalCapabilitySpec(norm.Spec)
	if err != nil {
		return nil, err
	}
	res, err := ex.execContext(ctx,
		`UPDATE capability_versions SET display_name = ?, spec = ?, resource_version = resource_version + 1
		 WHERE namespace_id = ? AND name = ? AND id = ? AND resource_version = ? AND deleted_at IS NULL`,
		updated.DisplayName, string(raw), cur.NamespaceID, cur.Name, cur.ID, cur.ResourceVersion)
	if err != nil {
		return nil, fmt.Errorf("update capability: %w", err)
	}
	if err := requireCASRow(res, cur.ID, cur.ResourceVersion); err != nil {
		return nil, err
	}
	if !sameStringList(cur.Spec.Next, norm.Spec.Next) {
		latest, err := getCapabilityLatestOn(ctx, ex, cur.NamespaceID, cur.Name)
		if err != nil {
			return nil, err
		}
		if latest.ID == cur.ID {
			if err := rebuildCapabilityEdgesOn(ctx, ex, cur.NamespaceID, cur.Name, norm.Spec.Next); err != nil {
				return nil, err
			}
		}
		if err := insertRoutingChangedEventOn(ctx, ex, cur, cur.Spec.Next, norm.Spec.Next); err != nil {
			return nil, err
		}
	}
	updated.Spec, updated.ResourceVersion = norm.Spec, cur.ResourceVersion+1
	return &updated, nil
}

// DeleteCapability hides all visible versions, retaining pinned bodies and incoming references.
func (d *DB) DeleteCapability(ctx context.Context, namespaceID, name, expectedVersion string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete capability: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	cur, err := getCapabilityLatestOn(ctx, ex, namespaceID, name)
	if err != nil {
		return err
	}
	if err := checkResourceVersion(expectedVersion, cur.ID, cur.ResourceVersion); err != nil {
		return err
	}
	res, err := ex.execContext(ctx, `UPDATE capability_versions SET deleted_at = ?, resource_version = resource_version + 1
		WHERE namespace_id = ? AND name = ? AND id = ? AND resource_version = ? AND deleted_at IS NULL`, Now(), namespaceID, name, cur.ID, cur.ResourceVersion)
	if err != nil {
		return fmt.Errorf("delete capability: %w", err)
	}
	if err := requireCASRow(res, cur.ID, cur.ResourceVersion); err != nil {
		return err
	}
	if _, err := ex.execContext(ctx, `UPDATE capability_versions SET deleted_at = ?, resource_version = resource_version + 1
		WHERE namespace_id = ? AND name = ? AND deleted_at IS NULL`, Now(), namespaceID, name); err != nil {
		return fmt.Errorf("delete capability versions: %w", err)
	}
	if _, err := ex.execContext(ctx, `DELETE FROM edges WHERE namespace_id = ? AND from_is_source = 0 AND from_ref = ?`, namespaceID, name); err != nil {
		return fmt.Errorf("delete capability projection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete capability: commit: %w", err)
	}
	return nil
}

// SourcePatch distinguishes omitted fields from explicit empty values; identity/kind are not writable.
type SourcePatch struct {
	DisplayName *string
	Config      []byte
	Targets     *[]sourcespec.Target
	Enabled     *bool
}

func (d *DB) UpdateSourceByName(ctx context.Context, namespaceID, name string, patch SourcePatch, expectedVersion string) (*Source, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update source by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	cur, err := getSourceByNameOn(ctx, ex, namespaceID, name)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, cur.ID, cur.ResourceVersion); err != nil {
		return nil, err
	}
	updated := *cur
	updated.Config = patch.Config
	if patch.DisplayName != nil {
		updated.DisplayName = *patch.DisplayName
	}
	if patch.Targets != nil {
		updated.Targets = *patch.Targets
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}
	if err := d.updateSourceOn(ctx, ex, cur, &updated, patch.Targets != nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update source by name: commit: %w", err)
	}
	return &updated, nil
}

func (d *DB) DeleteSourceByName(ctx context.Context, namespaceID, name, expectedVersion string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete source by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	cur, err := getSourceByNameOn(ctx, ex, namespaceID, name)
	if err != nil {
		return err
	}
	if err := checkResourceVersion(expectedVersion, cur.ID, cur.ResourceVersion); err != nil {
		return err
	}
	if err := deleteSourceOn(ctx, ex, cur); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete source by name: commit: %w", err)
	}
	return nil
}

// NodePatch contains only writable spec; name and machine/authorization facts stay untouched.
type NodePatch struct {
	Tags       *[]string
	Namespaces *[]string
	Vars       map[string]string
	Limits     map[string]any
}

func getNodeOn(ctx context.Context, ex queryExec, key, value string) (*Node, error) {
	// key is selected only by owned store entrypoints, never client SQL.
	if key != "id" && key != "name" {
		return nil, ErrNotFound
	}
	n, err := scanNodeRow(ex.queryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE "+key+" = ?", value))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func nodeConfigOn(ctx context.Context, ex queryExec, n *Node) (*NodeConfig, error) {
	cfg, err := scanNodeConfigOn(ctx, ex, n.ID)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = emptyNodeConfig(n.ID)
	}
	return cfg, nil
}

// UpdateNode shares the exact NodeConfig identity and revision with the config subresource.
func (d *DB) UpdateNode(ctx context.Context, name string, patch NodePatch, expectedVersion string) (*Node, error) {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update node: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	n, err := getNodeOn(ctx, ex, "name", name)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, n.ID, n.ResourceVersion); err != nil {
		return nil, err
	}
	cfg, err := nodeConfigOn(ctx, ex, n)
	if err != nil {
		return nil, err
	}
	if patch.Namespaces != nil {
		cfg.Namespaces = *patch.Namespaces
	}
	if patch.Vars != nil {
		cfg.Vars = patch.Vars
	}
	if patch.Limits != nil {
		cfg.Limits = patch.Limits
	}
	if err := putNodeConfigOn(ctx, ex, cfg, patch.Namespaces != nil || patch.Vars != nil || patch.Limits != nil); err != nil {
		return nil, err
	}
	if patch.Tags != nil {
		tags := append([]string{}, (*patch.Tags)...)
		raw, err := json.Marshal(tags)
		if err != nil {
			return nil, fmt.Errorf("update node tags: %w", err)
		}
		res, err := ex.execContext(ctx, "UPDATE nodes SET tags = ? WHERE id = ? AND name = ?", string(raw), n.ID, n.Name)
		if err != nil {
			return nil, fmt.Errorf("update node tags: %w", err)
		}
		if err := requireCASRow(res, n.ID, n.ResourceVersion); err != nil {
			return nil, err
		}
		n.Tags = tags
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update node: commit: %w", err)
	}
	n.ResourceVersion = cfg.ResourceVersion
	return n, nil
}

func (d *DB) PutNodeConfigByName(ctx context.Context, name string, cfg *NodeConfig, expectedVersion string) error {
	if cfg == nil {
		return fmt.Errorf("put node config by name: nil")
	}
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put node config by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	n, err := getNodeOn(ctx, ex, "name", name)
	if err != nil {
		return err
	}
	if err := checkResourceVersion(expectedVersion, n.ID, n.ResourceVersion); err != nil {
		return err
	}
	updated := *cfg
	updated.NodeID, updated.ResourceVersion = n.ID, n.ResourceVersion
	if err := putNodeConfigOn(ctx, ex, &updated, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("put node config by name: commit: %w", err)
	}
	*cfg = updated
	return nil
}

func deleteNodeOn(ctx context.Context, ex queryExec, n *Node) error {
	res, err := ex.execContext(ctx, `DELETE FROM nodes WHERE id = ? AND name = ? AND
 COALESCE((SELECT c.resource_version FROM node_configs c WHERE c.node_id = nodes.id),1) = ?`, n.ID, n.Name, n.ResourceVersion)
	if err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	return requireCASRow(res, n.ID, n.ResourceVersion)
}

func (d *DB) DeleteNodeByName(ctx context.Context, name, expectedVersion string) error {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete node by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	n, err := getNodeOn(ctx, ex, "name", name)
	if err != nil {
		return err
	}
	if err := checkResourceVersion(expectedVersion, n.ID, n.ResourceVersion); err != nil {
		return err
	}
	if err := deleteNodeOn(ctx, ex, n); err != nil {
		return err
	}
	return tx.Commit()
}

func getNamespaceOn(ctx context.Context, ex queryExec, key, value string) (*Namespace, error) {
	if key != "id" && key != "slug" {
		return nil, ErrNamespaceNotFound
	}
	var ns Namespace
	err := ex.queryRowContext(ctx, "SELECT "+nsColumns+" FROM namespaces WHERE "+key+" = ?", value).
		Scan(&ns.ID, &ns.Slug, &ns.Name, &ns.CreatedAt, &ns.ResourceVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNamespaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}
	return &ns, nil
}

func renameNamespaceOn(ctx context.Context, ex queryExec, ns *Namespace, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("rename namespace: empty name")
	}
	res, err := ex.execContext(ctx, "UPDATE namespaces SET name = ?, resource_version = resource_version + 1 WHERE id = ? AND slug = ? AND resource_version = ?", name, ns.ID, ns.Slug, ns.ResourceVersion)
	if err != nil {
		return fmt.Errorf("rename namespace: %w", err)
	}
	if err := requireCASRow(res, ns.ID, ns.ResourceVersion); err != nil {
		return err
	}
	ns.Name, ns.ResourceVersion = name, ns.ResourceVersion+1
	return nil
}

func (d *DB) UpdateNamespace(ctx context.Context, slug, displayName, expectedVersion string) (*Namespace, error) {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update namespace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	ns, err := getNamespaceOn(ctx, ex, "slug", slug)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, ns.ID, ns.ResourceVersion); err != nil {
		return nil, err
	}
	if err := renameNamespaceOn(ctx, ex, ns, displayName); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update namespace: commit: %w", err)
	}
	return ns, nil
}

func (d *DB) DeleteNamespaceByName(ctx context.Context, slug string, force bool, expectedVersion string) error {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete namespace by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	ns, err := getNamespaceOn(ctx, ex, "slug", slug)
	if err != nil {
		return err
	}
	if err := checkResourceVersion(expectedVersion, ns.ID, ns.ResourceVersion); err != nil {
		return err
	}
	if err := deleteNamespaceOn(ctx, ex, ns.ID, force); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) PutNamespaceConfigByName(ctx context.Context, slug string, vars map[string]string, expectedVersion string) (*NamespaceConfig, error) {
	if _, _, err := parseResourceVersion(expectedVersion); err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("put namespace config by name: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ex := txExec{tx: tx}
	ns, err := getNamespaceOn(ctx, ex, "slug", slug)
	if err != nil {
		return nil, err
	}
	if err := checkResourceVersion(expectedVersion, ns.ID, ns.ResourceVersion); err != nil {
		return nil, err
	}
	cfg, err := putNamespaceConfigOn(ctx, ex, ns.ID, vars, ns.ResourceVersion)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("put namespace config by name: commit: %w", err)
	}
	return cfg, nil
}
