// Package workspace is the durable blob store for agent-generated artifacts
// (generated PDFs/images/audio, downloaded files, intermediate data, ...).
//
// The two implementations currently shipped are:
//   - LocalFS: writes under ~/.fastclaw/workspaces/<agent>/. Default; used by
//     single-host deployments and keeps existing filesystem semantics intact.
//   - S3: writes to any S3-compatible bucket (AWS S3, MinIO, R2, B2, ...).
//     Required for stateless multi-pod deployments — any pod can read/write
//     the same object even though the filesystem is pod-local.
//
// Keep this package free of runtime deps on tools/handlers so sandbox code
// and agent code can share the same Store without pulling each other in.
package workspace

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store is the durable artifact store. Implementations MUST be concurrency-
// safe — two pods may hit the same key at once.
//
// Paths are agent-relative (e.g. "report.pdf", "images/cover.png"). Absolute
// paths are never passed in. Implementations are free to add their own
// namespacing (bucket prefix, directory tree, ...) below the agent scope.
type Store interface {
	// Put writes all bytes from r to the object at <agentID>/<path>. Any
	// prior object at that path is overwritten. If size is unknown pass -1.
	Put(ctx context.Context, agentID, path string, r io.Reader, size int64, contentType string) error

	// Get returns a reader for <agentID>/<path>. Caller MUST Close it.
	// Returns ErrNotFound when the object doesn't exist.
	Get(ctx context.Context, agentID, path string) (io.ReadCloser, error)

	// Stat returns metadata (size, content type, last modified) for an
	// object, or ErrNotFound.
	Stat(ctx context.Context, agentID, path string) (*ObjectInfo, error)

	// List enumerates every object under <agentID>/. Returns an empty slice
	// (not an error) when the agent has never written anything.
	List(ctx context.Context, agentID string) ([]ObjectInfo, error)

	// Delete removes a single object. No-op when it doesn't exist.
	Delete(ctx context.Context, agentID, path string) error

	// SignedURL returns a time-limited URL that lets the holder GET the
	// object over HTTPS without going through the gateway. Used by the
	// chat UI's download button to avoid streaming big blobs through the
	// gateway pod. Implementations that can't produce signed URLs (local
	// filesystem) return ErrSignedURLUnsupported.
	SignedURL(ctx context.Context, agentID, path string, ttl time.Duration) (string, error)
}

// ObjectInfo describes one stored object. Fields not known by a particular
// backend are zero.
type ObjectInfo struct {
	Path        string    // agent-relative
	Size        int64     // bytes, -1 when unknown
	ContentType string    // e.g. "image/png"
	ModTime     time.Time // UTC
}

// Common errors. Implementations should wrap these with fmt.Errorf("%w: ...")
// when adding context, so callers can still errors.Is() match.
var (
	ErrNotFound             = errors.New("workspace: object not found")
	ErrSignedURLUnsupported = errors.New("workspace: signed URLs not supported by this backend")
)
