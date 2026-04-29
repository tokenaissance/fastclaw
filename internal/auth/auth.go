// Package auth resolves an HTTP request to a user identity. It supports
// two credential types:
//   - cookie session ("fastclaw_session"): set by /api/login, validated
//     against the web_sessions table; used by the web UI
//   - Bearer apikey: validated against the apikeys table; used by API
//     consumers and CLI clients
//
// Both paths funnel into the same Identity struct stamped onto ctx via
// config.WithUserID. There is no anonymous "local" fallback — requests
// without valid credentials get 401.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// SessionCookieName is the cookie that backs the web UI's login state.
const SessionCookieName = "fastclaw_session"

// SessionTTL is how long a freshly-issued login cookie is valid.
const SessionTTL = 30 * 24 * time.Hour

// Identity is the resolved caller for one request.
type Identity struct {
	UserID string
	Role   string

	// AuthMethod is "session" or "apikey".
	AuthMethod string

	// APIKeyID is set when AuthMethod=="apikey". The auth-bound agent
	// access list lives on APIKeyAgents (nil = unlimited within the
	// owner's agents, which we never grant — the agent set is always
	// explicit when an apikey is created).
	APIKeyID      string
	APIKeyAgents  []string

	// ActAsUserID is non-empty when a super_admin is browsing another
	// user's resources read-only via ?actAs=. Mutating handlers MUST
	// 403 when this is set.
	ActAsUserID string
}

// EffectiveUserID is who we read data for. For super_admin in actAs mode
// it's the impersonated user; otherwise it's the caller themselves.
func (i Identity) EffectiveUserID() string {
	if i.ActAsUserID != "" {
		return i.ActAsUserID
	}
	return i.UserID
}

// IsActingAs reports whether super_admin is impersonating another user.
func (i Identity) IsActingAs() bool {
	return i.ActAsUserID != "" && i.ActAsUserID != i.UserID
}

// ReadOnly reports whether mutating endpoints must reject this request.
// Active actAs mode is the only read-only condition we enforce here.
func (i Identity) ReadOnly() bool {
	return i.IsActingAs()
}

// CanAccessAgent answers "is this caller authorized for agentID?"
//   - super_admin: yes, on any agent (read-only when in actAs mode)
//   - apikey: only if agentID ∈ APIKeyAgents
//   - session user: agent must belong to UserID (verified by the caller
//     querying agents table; we don't carry that list on Identity)
func (i Identity) CanAccessAgent(agentID string) bool {
	if i.Role == users.RoleSuperAdmin {
		return true
	}
	if i.AuthMethod == "apikey" {
		for _, a := range i.APIKeyAgents {
			if a == agentID {
				return true
			}
		}
		return false
	}
	// session caller: agent ownership check happens in the handler
	// after reading the agent row (cheap M:1 lookup, no list scan).
	return true
}

type identityKey struct{}

// WithIdentity stamps the resolved identity onto ctx so handlers can read
// it without re-validating credentials.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, identityKey{}, id)
	if uid := id.EffectiveUserID(); uid != "" {
		ctx = config.WithUserID(ctx, uid)
	}
	return ctx
}

// FromContext returns the resolved identity stamped by Middleware. The
// bool is false if no auth has run, which means a route is misconfigured
// (every API route must go through Middleware first).
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	v, ok := ctx.Value(identityKey{}).(Identity)
	return v, ok
}

// Resolver loads accounts, apikeys, and web sessions from the store.
type Resolver struct {
	store    store.Store
	apikeys  *users.APIKeys
	accounts *users.Accounts
}

// NewResolver returns a resolver bound to the platform store.
func NewResolver(st store.Store) (*Resolver, error) {
	if st == nil {
		return nil, errors.New("auth.NewResolver: store is required")
	}
	ak, err := users.NewAPIKeys(st)
	if err != nil {
		return nil, err
	}
	ac, err := users.NewAccounts(st)
	if err != nil {
		return nil, err
	}
	return &Resolver{store: st, apikeys: ak, accounts: ac}, nil
}

// IssueSession creates a web session for userID and returns the cookie.
// Caller writes the cookie to the response.
func (r *Resolver) IssueSession(ctx context.Context, userID string) (*http.Cookie, error) {
	sid, err := newSID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &store.WebSessionRecord{
		SID:       sid,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}
	if err := r.store.CreateWebSession(ctx, rec); err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  rec.ExpiresAt,
	}, nil
}

// RevokeSession drops a session from the store.
func (r *Resolver) RevokeSession(ctx context.Context, sid string) error {
	return r.store.DeleteWebSession(ctx, sid)
}

// ResolveSession turns a cookie SID into an Identity.
func (r *Resolver) ResolveSession(ctx context.Context, sid string) (Identity, error) {
	if sid == "" {
		return Identity{}, ErrUnauthorized
	}
	sess, err := r.store.GetWebSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = r.store.DeleteWebSession(ctx, sid)
		return Identity{}, ErrUnauthorized
	}
	user, err := r.accounts.Get(ctx, sess.UserID)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	if user.Status != users.StatusActive {
		return Identity{}, ErrUnauthorized
	}
	return Identity{
		UserID:     user.ID,
		Role:       user.Role,
		AuthMethod: "session",
	}, nil
}

// ResolveBearer turns a Bearer token into an Identity.
func (r *Resolver) ResolveBearer(ctx context.Context, token string) (Identity, error) {
	res, err := r.apikeys.LookupByToken(ctx, token)
	if err != nil {
		if errors.Is(err, users.ErrInvalidCredentials) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	return Identity{
		UserID:       res.Account.ID,
		Role:         res.Account.Role,
		AuthMethod:   "apikey",
		APIKeyID:     res.APIKey.ID,
		APIKeyAgents: append([]string(nil), res.Agents...),
	}, nil
}

// ErrUnauthorized is returned when no valid credential is present.
var ErrUnauthorized = errors.New("unauthorized")

// extract returns the bearer token (if any) and session cookie SID (if
// any) from a request. A `?token=` query param is also accepted for the
// rare cases (image previews, downloads) where setting headers isn't
// possible — same security posture as Authorization: Bearer.
func extract(r *http.Request) (bearer, sid string) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		sid = c.Value
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if t := strings.TrimPrefix(h, "Bearer "); t != h {
			bearer = t
		}
	} else if t := r.URL.Query().Get("token"); t != "" {
		bearer = t
	}
	return bearer, sid
}

// Middleware enforces auth on every wrapped route. 401 on no/invalid
// credentials. Resolves ?actAs= for super_admins.
func (r *Resolver) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		req = req.WithContext(WithIdentity(req.Context(), ident))
		next(w, req)
	}
}

// Optional resolves credentials when present but lets unauthenticated
// requests through. Used for /api/status during onboarding so the
// onboarding UI can probe the install before any user exists.
func (r *Resolver) Optional(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err == nil {
			req = req.WithContext(WithIdentity(req.Context(), ident))
		}
		next(w, req)
	}
}

func (r *Resolver) resolve(req *http.Request) (Identity, error) {
	bearer, sid := extract(req)

	var ident Identity
	var err error
	if sid != "" {
		ident, err = r.ResolveSession(req.Context(), sid)
		if err == nil {
			goto done
		}
	}
	if bearer != "" {
		ident, err = r.ResolveBearer(req.Context(), bearer)
		if err == nil {
			goto done
		}
	}
	return Identity{}, ErrUnauthorized

done:
	// actAs is reserved for super_admin and only applies to session
	// callers (apikey impersonation would defeat the apikey ACL).
	if act := req.URL.Query().Get("actAs"); act != "" {
		if ident.AuthMethod == "session" && ident.Role == users.RoleSuperAdmin {
			ident.ActAsUserID = act
		}
	}
	return ident, nil
}

// RequireSuperAdmin returns a middleware that 403s any non-super-admin
// caller. Wraps another middleware (typically the auth Middleware).
func RequireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok || ident.Role != users.RoleSuperAdmin {
			writeForbidden(w, "super_admin required")
			return
		}
		next(w, req)
	}
}

// RequireWritable rejects requests where Identity.ReadOnly() (i.e. the
// caller is acting as another user). Wrap mutating handlers.
func RequireWritable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok {
			writeUnauthorized(w)
			return
		}
		if ident.ReadOnly() {
			writeForbidden(w, "read-only: cannot mutate while acting as another user")
			return
		}
		next(w, req)
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"ok":false,"error":"` + msg + `"}`))
}

func newSID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
