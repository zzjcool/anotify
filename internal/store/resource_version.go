package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ResourceVersion encodes an immutable resource UID and its internal revision.
// HTTP metadata must use this opaque token, never the row counter alone.
func ResourceVersion(uid string, revision int64) string {
	return "rv1:" + uid + ":" + strconv.FormatInt(revision, 10)
}

const CodeResourceVersionInvalid = "resource_version_invalid"

var ErrResourceVersionInvalid = errors.New("store: resource version invalid")

// ResourceVersionConflictError exposes the complete current token for Status.details.current.
type ResourceVersionConflictError struct {
	CurrentResourceVersion string
}

func (e *ResourceVersionConflictError) Error() string {
	return fmt.Sprintf("%s (current %s)", ErrResourceVersionConflict, e.CurrentResourceVersion)
}
func (e *ResourceVersionConflictError) Unwrap() error { return ErrResourceVersionConflict }
func (e *ResourceVersionConflictError) Code() string  { return CodeResourceVersionConflict }

func parseResourceVersion(token string) (string, int64, error) {
	if token == "" {
		return "", 0, ErrResourceVersionRequired
	}
	parts := strings.Split(token, ":")
	if len(parts) != 3 || parts[0] != "rv1" || parts[1] == "" || strings.ContainsAny(parts[1], " \t\r\n\x00") {
		return "", 0, ErrResourceVersionInvalid
	}
	rev, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || rev <= 0 || strconv.FormatInt(rev, 10) != parts[2] {
		return "", 0, ErrResourceVersionInvalid
	}
	return parts[1], rev, nil
}

func checkResourceVersion(token, uid string, revision int64) error {
	expectedUID, expectedRevision, err := parseResourceVersion(token)
	if err != nil {
		return err
	}
	if expectedUID != uid || expectedRevision != revision {
		return resourceVersionConflict(uid, revision)
	}
	return nil
}

func resourceVersionConflict(uid string, revision int64) error {
	return &ResourceVersionConflictError{CurrentResourceVersion: ResourceVersion(uid, revision)}
}

// Internal immutable-ID callers may use a counter; public name paths must use the token.
func checkResourceRevision(expected int64, uid string, revision int64) error {
	if expected <= 0 {
		return ErrResourceVersionRequired
	}
	if expected != revision {
		return resourceVersionConflict(uid, revision)
	}
	return nil
}
