package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/zzjcool/anotify/internal/protocol"
)

// These errors belong to the existing artifact action, not a new resource kind.
var (
	ErrArtifactPathInvalid   = errors.New(protocol.ArtifactErrPathInvalid)
	ErrArtifactHashMismatch  = errors.New(protocol.ArtifactErrHashMismatch)
	ErrArtifactTooLarge      = errors.New(protocol.ArtifactErrTooLarge)
	ErrArtifactConflict      = errors.New(protocol.ArtifactErrConflict)
	ErrArtifactReference     = errors.New("artifact_reference_not_registered")
	ErrArtifactExpired       = errors.New("artifact_expired")
	ErrArtifactUploadExpired = errors.New("artifact_upload_expired")
)

// ArtifactObjectID is minted by the store, never from a caller path/hash. G3 uses
// String ONLY beneath its private system root. There is no string-to-ID constructor.
type ArtifactObjectID struct{ id string }

func (id ArtifactObjectID) String() string { return id.id }

// ArtifactUpload is a bounded in-progress publication. The opaque ticket and
// object identity survive network I/O, but no DB transaction does. G3 must verify
// the actual stream, atomically publish this object, then RegisterArtifactUpload;
// on rejection, abort the ticket and collect only revoked IDs via GC (never
// blindly unlink a previously accepted object on retry). Store does no I/O.
type ArtifactUpload struct {
	id       string
	object   ArtifactObjectID
	response protocol.ArtifactUploadResponse
}

func (u *ArtifactUpload) ObjectID() ArtifactObjectID                { return u.object }
func (u *ArtifactUpload) Response() protocol.ArtifactUploadResponse { return u.response }

// ArtifactManifestEntry is persisted metadata, including expiry after bytes GC.
// ObjectID is internal system identity; never expose it in task JSON.
type ArtifactManifestEntry struct {
	TaskID       string           `json:"taskId"`
	Attempt      int              `json:"attempt"`
	NamespaceID  string           `json:"-"`
	CapabilityID string           `json:"-"`
	Path         string           `json:"path"`
	SHA256       string           `json:"sha256"`
	Size         int64            `json:"size"`
	ObjectID     ArtifactObjectID `json:"-"`
	PublishedAt  int64            `json:"publishedAt"`
	ExpiresAt    int64            `json:"expiresAt"`
	ExpiredAt    *int64           `json:"expiredAt"`
	ResultRef    bool             `json:"-"`
}

func validateArtifactInput(in protocol.ArtifactUploadResponse) error {
	if in.TaskID == "" || in.Attempt <= 0 {
		return ErrLeaseMismatch
	}
	if !protocol.ValidArtifactLogicalPath(in.Path) {
		return ErrArtifactPathInvalid
	}
	if !protocol.ValidArtifactSHA256(in.SHA256) {
		return ErrArtifactHashMismatch
	}
	if in.Size < 0 {
		return fmt.Errorf("artifact size must be nonnegative")
	}
	if in.Size > protocol.MaxArtifactTransferBytes {
		return ErrArtifactTooLarge
	}
	return nil
}

func uploadAuthOn(ctx context.Context, ex queryExec, auth EmitAuth, in protocol.ArtifactUploadResponse) error {
	if auth.TaskID != in.TaskID || auth.Attempt != in.Attempt || auth.AttemptToken == "" || auth.LeaseID == "" {
		return ErrAttemptTokenInvalid
	}
	return authorizeEmitOn(ctx, ex, auth)
}

// BeginArtifactUpload prechecks current credential, namespace, lease and token,
// then records a 30s active ticket protecting staging/metadata from GC. Final
// registration repeats ALL authorization in its own writer transaction.
func (d *DB) BeginArtifactUpload(ctx context.Context, auth EmitAuth, in protocol.ArtifactUploadResponse) (*ArtifactUpload, error) {
	if err := validateArtifactInput(in); err != nil {
		return nil, err
	}
	if auth.Now == 0 {
		auth.Now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin artifact upload: %w", err)
	}
	defer tx.Rollback()
	ex := txExec{tx: tx}
	if err := uploadAuthOn(ctx, ex, auth, in); err != nil {
		return nil, err
	}
	if _, _, err := registeredArtifactOn(ctx, ex, in); err != nil {
		return nil, err
	}
	u := &ArtifactUpload{id: newID("upl"), object: ArtifactObjectID{id: newID("obj")}, response: in}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_artifact_objects(id,created_at) VALUES (?,?)`, u.object.id, auth.Now); err != nil {
		return nil, fmt.Errorf("artifact object: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_artifact_uploads(id,task_id,attempt,path,sha256,size,object_id,expires_at) VALUES (?,?,?,?,?,?,?,?)`, u.id, in.TaskID, in.Attempt, in.Path, in.SHA256, in.Size, u.object.id, auth.Now+30); err != nil {
		return nil, fmt.Errorf("artifact ticket: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("begin artifact commit: %w", err)
	}
	return u, nil
}

// registeredArtifactOn checks an exact logical key; identical bytes/size are
// idempotent, all other reuse conflicts. Authorization MUST run before this.
func registeredArtifactOn(ctx context.Context, ex queryExec, in protocol.ArtifactUploadResponse) (ArtifactObjectID, bool, error) {
	var hash, id string
	var size int64
	err := ex.queryRowContext(ctx, `SELECT sha256,size,object_id FROM task_artifact_manifests WHERE task_id=? AND attempt=? AND path=?`, in.TaskID, in.Attempt, in.Path).Scan(&hash, &size, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return ArtifactObjectID{}, false, nil
	}
	if err != nil {
		return ArtifactObjectID{}, false, fmt.Errorf("read registered artifact: %w", err)
	}
	if hash != in.SHA256 || size != in.Size {
		return ArtifactObjectID{}, false, ErrArtifactConflict
	}
	return ArtifactObjectID{id: id}, true, nil
}

// ArtifactUploadRegistration gives G3 the actual owner identity from the SAME
// registration transaction. A duplicate spool is not necessarily the winner;
// never blindly remove u.ObjectID on 200 or on a late authorization failure.
type ArtifactUploadRegistration struct {
	Response          protocol.ArtifactUploadResponse
	Status            int
	PublishedObjectID ArtifactObjectID
}

// RegisterArtifactUploadClassified exposes atomic registration ownership for G3.
func (d *DB) RegisterArtifactUploadClassified(ctx context.Context, auth EmitAuth, u *ArtifactUpload, actualSHA256 string, actualSize int64) (ArtifactUploadRegistration, error) {
	var out ArtifactUploadRegistration
	var err error
	out.Response, out.Status, err = d.registerArtifactUpload(ctx, auth, u, actualSHA256, actualSize, &out.PublishedObjectID)
	return out, err
}

// RegisterArtifactUpload accepts ONLY a ticket minted by Begin, actual verified
// size/hash, and CURRENT session+token authorization. First acceptance is 201;
// exact same task/attempt/path/hash/size is 200 without charging quota twice.
// Quota counts all registered logical items inside the same writer transaction.
func (d *DB) RegisterArtifactUpload(ctx context.Context, auth EmitAuth, u *ArtifactUpload, actualSHA256 string, actualSize int64) (protocol.ArtifactUploadResponse, int, error) {
	out, err := d.RegisterArtifactUploadClassified(ctx, auth, u, actualSHA256, actualSize)
	return out.Response, out.Status, err
}

func (d *DB) registerArtifactUpload(ctx context.Context, auth EmitAuth, u *ArtifactUpload, actualSHA256 string, actualSize int64, publishedObject *ArtifactObjectID) (protocol.ArtifactUploadResponse, int, error) {
	var empty protocol.ArtifactUploadResponse
	if u == nil || u.id == "" || u.object.id == "" {
		return empty, 0, ErrArtifactUploadExpired
	}
	in := u.response
	if err := validateArtifactInput(in); err != nil {
		return empty, 0, err
	}
	if actualSize < 0 || actualSize > protocol.MaxArtifactTransferBytes {
		return empty, 0, ErrArtifactTooLarge
	}
	if actualSHA256 != in.SHA256 || actualSize != in.Size {
		return empty, 0, ErrArtifactHashMismatch
	}
	if auth.Now == 0 {
		auth.Now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return empty, 0, fmt.Errorf("register artifact begin: %w", err)
	}
	defer tx.Rollback()
	ex := txExec{tx: tx}
	if err := uploadAuthOn(ctx, ex, auth, in); err != nil {
		return empty, 0, err
	}
	registeredObject, exists, err := registeredArtifactOn(ctx, ex, in)
	if err != nil {
		return empty, 0, err
	}
	if exists {
		// The new spool is not a manifest owner. Its object stays GC-visible.
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_artifact_uploads WHERE id=?`, u.id); err != nil {
			return empty, 0, fmt.Errorf("finish duplicate upload: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return empty, 0, err
		}
		*publishedObject = registeredObject
		return in, protocol.ArtifactUploadStatusOK, nil
	}
	var expiry int64
	err = tx.QueryRowContext(ctx, `SELECT expires_at FROM task_artifact_uploads WHERE id=? AND task_id=? AND attempt=? AND path=? AND sha256=? AND size=? AND object_id=?`, u.id, in.TaskID, in.Attempt, in.Path, in.SHA256, in.Size, u.object.id).Scan(&expiry)
	if errors.Is(err, sql.ErrNoRows) || err == nil && expiry <= auth.Now {
		return empty, 0, ErrArtifactUploadExpired
	}
	if err != nil {
		return empty, 0, fmt.Errorf("read upload ticket: %w", err)
	}
	var items int
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0) FROM task_artifact_manifests WHERE task_id=? AND attempt=?`, in.TaskID, in.Attempt).Scan(&items, &total); err != nil {
		return empty, 0, fmt.Errorf("artifact quota: %w", err)
	}
	if items >= protocol.MaxArtifactTransferItems || total+in.Size > protocol.MaxAttemptArtifactBytes {
		return empty, 0, ErrArtifactTooLarge
	}
	var ns, cap string
	if err := tx.QueryRowContext(ctx, `SELECT namespace_id,capability_id FROM tasks WHERE id=?`, in.TaskID).Scan(&ns, &cap); err != nil {
		return empty, 0, err
	}
	ttl := DefaultRetentionResultTTLSec
	if err := tx.QueryRowContext(ctx, `SELECT retention_result_ttl_sec FROM system_config WHERE id=1`).Scan(&ttl); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return empty, 0, fmt.Errorf("artifact TTL: %w", err)
	}
	expires := int64(0)
	if ttl > 0 {
		expires = auth.Now + ttl
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_artifact_manifests(task_id,attempt,namespace_id,capability_id,path,sha256,size,object_id,published_at,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, in.TaskID, in.Attempt, ns, cap, in.Path, in.SHA256, in.Size, u.object.id, auth.Now, expires); err != nil {
		return empty, 0, fmt.Errorf("register artifact: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_artifact_uploads WHERE id=?`, u.id); err != nil {
		return empty, 0, err
	}
	if err := tx.Commit(); err != nil {
		return empty, 0, fmt.Errorf("register artifact commit: %w", err)
	}
	*publishedObject = u.object
	return in, protocol.ArtifactUploadStatusCreated, nil
}

// AbortArtifactUpload relinquishes this ticket only; any registered manifest
// remains untouched. Its orphan system object is returned by the bounded GC.
func (d *DB) AbortArtifactUpload(ctx context.Context, u *ArtifactUpload) error {
	if u == nil || u.id == "" {
		return ErrArtifactUploadExpired
	}
	_, err := d.ExecContext(ctx, `DELETE FROM task_artifact_uploads WHERE id=? AND object_id=?`, u.id, u.object.id)
	if err != nil {
		return fmt.Errorf("abort artifact upload: %w", err)
	}
	return nil
}

func canonicalManifest(in []protocol.TaskArtifact) ([]protocol.TaskArtifact, error) {
	out := append([]protocol.TaskArtifact{}, in...)
	if len(out) > protocol.MaxArtifactTransferItems {
		return nil, ErrArtifactTooLarge
	}
	seen := map[string]bool{}
	for _, a := range out {
		if !protocol.ValidArtifactLogicalPath(a.Path) {
			return nil, ErrArtifactPathInvalid
		}
		if !protocol.ValidArtifactSHA256(a.SHA256) {
			return nil, ErrArtifactHashMismatch
		}
		if seen[a.Path] {
			return nil, ErrArtifactConflict
		}
		seen[a.Path] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func uploadedManifestOn(ctx context.Context, ex queryExec, taskID string, attempt int) ([]protocol.TaskArtifact, error) {
	if attempt == 0 {
		if err := ex.queryRowContext(ctx, `SELECT attempt FROM tasks WHERE id=?`, taskID).Scan(&attempt); err != nil {
			return nil, fmt.Errorf("artifact current attempt: %w", err)
		}
	}
	rows, err := ex.queryContext(ctx, `SELECT path,sha256 FROM task_artifact_manifests WHERE task_id=? AND attempt=? ORDER BY path`, taskID, attempt)
	if err != nil {
		return nil, fmt.Errorf("uploaded manifest: %w", err)
	}
	defer rows.Close()
	out := []protocol.TaskArtifact{}
	for rows.Next() {
		var a protocol.TaskArtifact
		if err := rows.Scan(&a.Path, &a.SHA256); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func validateResultManifestOn(ctx context.Context, ex queryExec, taskID string, attempt int, result TaskResult) (TaskResult, error) {
	refs, err := canonicalManifest(result.Artifacts)
	if err != nil {
		return result, err
	}
	uploaded, err := uploadedManifestOn(ctx, ex, taskID, attempt)
	if err != nil {
		return result, err
	}
	byPath := map[string]string{}
	for _, a := range uploaded {
		byPath[a.Path] = a.SHA256
	}
	for _, a := range refs {
		if byPath[a.Path] != a.SHA256 {
			return result, ErrArtifactReference
		}
	}
	// Export/transfer failures must retain every successfully uploaded partial ref.
	if result.Reason == protocol.ReasonArtifactUploadFailed && !reflect.DeepEqual(refs, uploaded) {
		return result, ErrArtifactReference
	}
	result.Artifacts = refs
	return result, nil
}

// ListArtifactManifest reads exact retained attempt metadata including expired
// copies. Empty lists are real [], not a fabricated success from missing bytes.
func (d *DB) ListArtifactManifest(ctx context.Context, namespaceID, taskID string, attempt int) ([]ArtifactManifestEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx, `SELECT task_id,attempt,namespace_id,capability_id,path,sha256,size,object_id,published_at,expires_at,expired_at,result_ref FROM task_artifact_manifests WHERE namespace_id=? AND task_id=? AND attempt=? ORDER BY path`, namespaceID, taskID, attempt)
	if err != nil {
		return nil, fmt.Errorf("list artifact manifest: %w", err)
	}
	defer rows.Close()
	out := []ArtifactManifestEntry{}
	for rows.Next() {
		var a ArtifactManifestEntry
		var id string
		var ref int
		if err := rows.Scan(&a.TaskID, &a.Attempt, &a.NamespaceID, &a.CapabilityID, &a.Path, &a.SHA256, &a.Size, &id, &a.PublishedAt, &a.ExpiresAt, &a.ExpiredAt, &ref); err != nil {
			return nil, err
		}
		a.ObjectID = ArtifactObjectID{id: id}
		a.ResultRef = ref != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetTaskArtifactDownload checks the TASK RESULT reference, not a global digest.
// Caller supplies an already user-authorized immutable namespace ID. 404 means
// no result ref; ErrArtifactExpired means retained metadata with no system copy.
// G3 must open only the returned opaque system object (never node fallback).
//
// Selection (G3-D4): the SAME sha256 may be referenced by several logical paths of
// the current reported result. Any still-live copy wins — the first live entry in
// canonical path order (ListArtifactManifest is ORDER BY path) is returned so the
// download can hand out a deterministic safe basename. Only when every matching
// result reference is expired does this report ErrArtifactExpired. A leading
// expired row must never mask a later live one (that was the pre-#332 defect).
// Borrowed bytes are impossible: the scan is restricted to this taskId + its
// current reported attempt + result_ref rows, so another task/namespace/attempt
// holding a live copy of the same hash is invisible here.
func (d *DB) GetTaskArtifactDownload(ctx context.Context, namespaceID, taskID, sha256 string, now int64) (*ArtifactManifestEntry, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if !protocol.ValidArtifactSHA256(sha256) {
		return nil, ErrNotFound
	}
	if now == 0 {
		now = Now()
	}
	var attempt int
	if err := d.QueryRowContext(ctx, `SELECT attempt FROM tasks WHERE id=? AND namespace_id=? AND reported=1`, taskID, namespaceID).Scan(&attempt); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	entries, err := d.ListArtifactManifest(ctx, namespaceID, taskID, attempt)
	if err != nil {
		return nil, err
	}
	expired := false
	for i := range entries {
		a := &entries[i]
		if a.SHA256 != sha256 || !a.ResultRef {
			continue
		}
		if a.ExpiredAt == nil && (a.ExpiresAt == 0 || a.ExpiresAt > now) {
			return a, nil
		}
		expired = true
	}
	if expired {
		return nil, ErrArtifactExpired
	}
	return nil, ErrNotFound
}

// ExpireArtifactObjects atomically revokes system byte identities and retains
// manifest metadata for 410. No active ticket or unreaped/running attempt may be
// collected. Registration cannot attach an expired identity: only a live ticket
// minted for a new object can publish. Returned IDs are retryable purge work;
// G3 removes those private system files outside the transaction, then acknowledges.
func (d *DB) ExpireArtifactObjects(ctx context.Context, now int64, limit int) ([]ArtifactObjectID, error) {
	out := []ArtifactObjectID{}
	if limit <= 0 {
		return out, nil
	}
	if now == 0 {
		now = Now()
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_artifact_uploads WHERE id IN (SELECT id FROM task_artifact_uploads WHERE expires_at<=? ORDER BY expires_at,id LIMIT ?)`, now, limit); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM task_artifact_objects o WHERE o.purged_at IS NULL AND
 NOT EXISTS(SELECT 1 FROM task_artifact_uploads u WHERE u.object_id=o.id) AND
 NOT EXISTS(SELECT 1 FROM task_artifact_manifests m JOIN tasks t ON t.id=m.task_id WHERE m.object_id=o.id AND (m.expires_at=0 OR m.expires_at>? OR t.phase IN ('pending','running') OR EXISTS(SELECT 1 FROM task_artifact_uploads u WHERE u.task_id=t.id) OR EXISTS(SELECT 1 FROM task_attempts a WHERE a.task_id=t.id AND a.reaped_at IS NULL)))
 ORDER BY o.created_at,o.id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ArtifactObjectID{id: id})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE task_artifact_objects SET expired_at=COALESCE(expired_at,?) WHERE id=?`, now, id.id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_artifact_manifests SET expired_at=COALESCE(expired_at,?) WHERE object_id=?`, now, id.id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// AckArtifactObjectPurged records completion only for already-revoked IDs.
func (d *DB) AckArtifactObjectPurged(ctx context.Context, id ArtifactObjectID, now int64) error {
	if id.id == "" {
		return ErrNotFound
	}
	if now == 0 {
		now = Now()
	}
	res, err := d.ExecContext(ctx, `UPDATE task_artifact_objects SET purged_at=COALESCE(purged_at,?) WHERE id=? AND expired_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM task_artifact_uploads u WHERE u.object_id=task_artifact_objects.id)`, now, id.id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}
