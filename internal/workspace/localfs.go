package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalFS stores objects under a root directory, one subtree per agent. This
// is the default backend for single-host deployments — same on-disk layout
// the agent tools already use, so existing agents upgrade in place.
type LocalFS struct {
	// root is usually ~/.fastclaw/workspaces. Objects for agent foo go to
	// <root>/foo/<path>.
	root string
}

// NewLocalFS returns a LocalFS rooted at the given directory. The directory
// is created on first Put; callers don't need to pre-create it.
func NewLocalFS(root string) *LocalFS {
	return &LocalFS{root: root}
}

func (f *LocalFS) agentDir(agentID string) string {
	return filepath.Join(f.root, agentID)
}

// resolvePath joins agentDir with path and rejects attempts to escape via
// "..". Any symbolic link inside the agent dir is left alone — escape via
// symlinks is a filesystem-level trust boundary users control.
func (f *LocalFS) resolvePath(agentID, path string) (string, error) {
	dir := f.agentDir(agentID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absDir, filepath.Clean("/"+path)) // strip leading ../
	if full != absDir && !strings.HasPrefix(full, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace: path %q escapes agent root", path)
	}
	return full, nil
}

func (f *LocalFS) Put(ctx context.Context, agentID, path string, r io.Reader, _ int64, _ string) error {
	full, err := f.resolvePath(agentID, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (f *LocalFS) Get(ctx context.Context, agentID, path string) (io.ReadCloser, error) {
	full, err := f.resolvePath(agentID, path)
	if err != nil {
		return nil, err
	}
	rc, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return rc, err
}

func (f *LocalFS) Stat(ctx context.Context, agentID, path string) (*ObjectInfo, error) {
	full, err := f.resolvePath(agentID, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ObjectInfo{
		Path:        path,
		Size:        info.Size(),
		ContentType: mime.TypeByExtension(filepath.Ext(path)),
		ModTime:     info.ModTime().UTC(),
	}, nil
}

func (f *LocalFS) List(ctx context.Context, agentID string) ([]ObjectInfo, error) {
	dir := f.agentDir(agentID)
	var out []ObjectInfo
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipAll // no workspace yet; equivalent to empty
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{
			Path:        filepath.ToSlash(rel),
			Size:        info.Size(),
			ContentType: mime.TypeByExtension(filepath.Ext(p)),
			ModTime:     info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

func (f *LocalFS) Delete(ctx context.Context, agentID, path string) error {
	full, err := f.resolvePath(agentID, path)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SignedURL is not supported for local files — there's nothing to sign. Call
// sites that need to hand a URL to a browser should fall through to the
// gateway's existing /api/agents/{id}/files/{path} endpoint, which streams
// the file over the authenticated channel.
func (f *LocalFS) SignedURL(ctx context.Context, agentID, path string, ttl time.Duration) (string, error) {
	return "", ErrSignedURLUnsupported
}
