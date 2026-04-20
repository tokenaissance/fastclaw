package workspace

import (
	"fmt"
	"path/filepath"
)

// Factory builds the configured Store. Pulled out of the constructors so a
// single config-driven entry point sits between gateway startup and the
// actual backend implementations.
type Factory struct {
	Backend  string
	LocalDir string
	S3       S3Config
}

// New returns the Store described by f. Unknown backends default to local
// filesystem so misconfiguration degrades gracefully instead of blowing up
// at startup.
func (f Factory) New(defaultLocalDir string) (Store, error) {
	switch f.Backend {
	case "", "local":
		root := f.LocalDir
		if root == "" {
			root = defaultLocalDir
		}
		return NewLocalFS(filepath.Clean(root)), nil
	case "s3":
		return NewS3(f.S3)
	default:
		return nil, fmt.Errorf("workspace: unknown backend %q", f.Backend)
	}
}
