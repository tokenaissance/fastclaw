// Package usage — quota enforcement layer.
//
// Quotas are per-user monthly token ceilings set by upstream SaaS apps
// (e.g. weclaw) via PUT /v1/quota.  The agent loop checks CheckQuota
// before every LLM call; channel messages (WeChat, Telegram, …) that
// arrive when the user is over-limit get a friendly rejection instead
// of burning tokens the app can't bill for.
//
// Two implementations mirror Meter: MemQuotaStore (dev/test) and
// SQLQuotaStore (prod, backed by the quotas table).
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Quota is one user's monthly ceiling.
type Quota struct {
	UserID             string `json:"userId"`
	MonthlyTokenLimit  int64  `json:"monthlyTokenLimit"`  // 0 = unlimited
	MonthlyRequestLimit int64 `json:"monthlyRequestLimit"` // 0 = unlimited
	ResetDay           int    `json:"resetDay"`            // 1–28; day of month the window resets
}

// QuotaStatus is the result of a quota check.
type QuotaStatus struct {
	Allowed           bool   `json:"allowed"`
	MonthlyTokenLimit int64  `json:"monthlyTokenLimit"`
	MonthlyRequestLimit int64 `json:"monthlyRequestLimit"`
	TokensUsed        int64  `json:"tokensUsed"`
	RequestsUsed      int64  `json:"requestsUsed"`
	ResetsAt          string `json:"resetsAt"` // RFC3339 date of next reset
}

// QuotaStore is the persistence interface for quotas.
type QuotaStore interface {
	GetQuota(ctx context.Context, userID string) (*Quota, error)
	SetQuota(ctx context.Context, q *Quota) error
	DeleteQuota(ctx context.Context, userID string) error
}

// currentBillingWindow returns the [start, end) day range for the
// billing period that contains `now`, given a reset day-of-month.
func currentBillingWindow(now time.Time, resetDay int) (since, until time.Time) {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	y, m, d := now.UTC().Date()
	if d >= resetDay {
		since = time.Date(y, m, resetDay, 0, 0, 0, 0, time.UTC)
		until = time.Date(y, m+1, resetDay-1, 0, 0, 0, 0, time.UTC)
	} else {
		since = time.Date(y, m-1, resetDay, 0, 0, 0, 0, time.UTC)
		until = time.Date(y, m, resetDay-1, 0, 0, 0, 0, time.UTC)
	}
	return since, until
}

// CheckQuota reads the quota + current usage and returns whether the
// user is allowed to proceed. Returns Allowed=true when no quota is
// configured (unlimited).
func CheckQuota(ctx context.Context, qs QuotaStore, meter Meter, userID string) (*QuotaStatus, error) {
	q, err := qs.GetQuota(ctx, userID)
	if err != nil {
		// No quota row → unlimited.
		return &QuotaStatus{Allowed: true}, nil
	}
	if q.MonthlyTokenLimit <= 0 && q.MonthlyRequestLimit <= 0 {
		return &QuotaStatus{Allowed: true}, nil
	}

	since, until := currentBillingWindow(time.Now(), q.ResetDay)
	r := Range{Since: since, Until: until}
	totals, err := meter.TotalsForUser(ctx, userID, r)
	if err != nil {
		// Metering failure should not block the user.
		return &QuotaStatus{Allowed: true}, nil
	}

	tokensUsed := totals.Input + totals.Output + totals.CacheRead + totals.CacheCreation
	allowed := true
	if q.MonthlyTokenLimit > 0 && tokensUsed >= q.MonthlyTokenLimit {
		allowed = false
	}
	if q.MonthlyRequestLimit > 0 && totals.Requests >= q.MonthlyRequestLimit {
		allowed = false
	}

	resetsAt := until.AddDate(0, 0, 1).Format(time.RFC3339)

	return &QuotaStatus{
		Allowed:             allowed,
		MonthlyTokenLimit:   q.MonthlyTokenLimit,
		MonthlyRequestLimit: q.MonthlyRequestLimit,
		TokensUsed:          tokensUsed,
		RequestsUsed:        totals.Requests,
		ResetsAt:            resetsAt,
	}, nil
}

// --------------------------------------------------------------------
// MemQuotaStore
// --------------------------------------------------------------------

// MemQuotaStore is an in-memory quota store for dev/test.
type MemQuotaStore struct {
	mu     sync.Mutex
	quotas map[string]*Quota
}

func NewMemQuotaStore() *MemQuotaStore {
	return &MemQuotaStore{quotas: make(map[string]*Quota)}
}

func (m *MemQuotaStore) GetQuota(_ context.Context, userID string) (*Quota, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.quotas[userID]
	if !ok {
		return nil, fmt.Errorf("no quota for user %s", userID)
	}
	return q, nil
}

func (m *MemQuotaStore) SetQuota(_ context.Context, q *Quota) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[q.UserID] = q
	return nil
}

func (m *MemQuotaStore) DeleteQuota(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.quotas, userID)
	return nil
}

// --------------------------------------------------------------------
// SQLQuotaStore
// --------------------------------------------------------------------

// SQLQuotaStore persists quotas in the quotas table.
type SQLQuotaStore struct {
	db      *sql.DB
	dialect string
}

func NewSQLQuotaStore(db *sql.DB, dialect string) *SQLQuotaStore {
	return &SQLQuotaStore{db: db, dialect: dialect}
}

func (s *SQLQuotaStore) rebind(q string) string {
	if s.dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (s *SQLQuotaStore) GetQuota(ctx context.Context, userID string) (*Quota, error) {
	q := s.rebind(`SELECT user_id, monthly_token_limit, monthly_request_limit, reset_day FROM quotas WHERE user_id = ?`)
	row := s.db.QueryRowContext(ctx, q, userID)
	var out Quota
	if err := row.Scan(&out.UserID, &out.MonthlyTokenLimit, &out.MonthlyRequestLimit, &out.ResetDay); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("no quota for user %s", userID)
		}
		return nil, err
	}
	return &out, nil
}

func (s *SQLQuotaStore) SetQuota(ctx context.Context, q *Quota) error {
	query := s.rebind(`
		INSERT INTO quotas (user_id, monthly_token_limit, monthly_request_limit, reset_day, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (user_id) DO UPDATE SET
			monthly_token_limit = EXCLUDED.monthly_token_limit,
			monthly_request_limit = EXCLUDED.monthly_request_limit,
			reset_day = EXCLUDED.reset_day,
			updated_at = CURRENT_TIMESTAMP`)
	_, err := s.db.ExecContext(ctx, query, q.UserID, q.MonthlyTokenLimit, q.MonthlyRequestLimit, q.ResetDay)
	return err
}

func (s *SQLQuotaStore) DeleteQuota(ctx context.Context, userID string) error {
	q := s.rebind(`DELETE FROM quotas WHERE user_id = ?`)
	_, err := s.db.ExecContext(ctx, q, userID)
	return err
}
