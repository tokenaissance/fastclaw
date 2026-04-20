package workspace

import (
	"context"
	"io"
	"time"
)

// MeterFunc is called for every successful Put with the number of bytes
// written. Kept as a plain function value (not an interface) so the
// workspace package stays free of a dep on internal/usage — the gateway
// wires the two together at startup.
type MeterFunc func(ctx context.Context, agentID string, bytes int64)

// Metered wraps an existing Store to count bytes flowing through Put.
// Get / Stat / List / Delete / SignedURL pass through untouched.
type Metered struct {
	inner Store
	meter MeterFunc
}

// NewMetered returns a Store that tallies write volume per agent. meter
// must be non-nil; use the underlying Store directly if you don't want
// metering.
func NewMetered(inner Store, meter MeterFunc) *Metered {
	return &Metered{inner: inner, meter: meter}
}

// countingReader forwards bytes through and tallies them. Needed because
// Put accepts an io.Reader (not a []byte) — we can't know the payload
// size without either trusting the caller's `size` hint or counting as we
// read.
type countingReader struct {
	r       io.Reader
	n       int64
	doneErr error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF {
		c.doneErr = err
	}
	return n, err
}

func (m *Metered) Put(ctx context.Context, agentID, path string, r io.Reader, size int64, contentType string) error {
	cr := &countingReader{r: r}
	if err := m.inner.Put(ctx, agentID, path, cr, size, contentType); err != nil {
		return err
	}
	// Prefer the caller's size when it's reliable; otherwise fall back
	// to the byte count we observed. Size=-1 means "don't know", which
	// is when the counting fallback matters most.
	n := size
	if n < 0 {
		n = cr.n
	}
	m.meter(ctx, agentID, n)
	return nil
}

func (m *Metered) Get(ctx context.Context, agentID, path string) (io.ReadCloser, error) {
	return m.inner.Get(ctx, agentID, path)
}

func (m *Metered) Stat(ctx context.Context, agentID, path string) (*ObjectInfo, error) {
	return m.inner.Stat(ctx, agentID, path)
}

func (m *Metered) List(ctx context.Context, agentID string) ([]ObjectInfo, error) {
	return m.inner.List(ctx, agentID)
}

func (m *Metered) Delete(ctx context.Context, agentID, path string) error {
	return m.inner.Delete(ctx, agentID, path)
}

func (m *Metered) SignedURL(ctx context.Context, agentID, path string, ttl time.Duration) (string, error) {
	return m.inner.SignedURL(ctx, agentID, path, ttl)
}

// Compile-time check.
var _ Store = (*Metered)(nil)
