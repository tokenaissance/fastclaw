package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// HandleGetUsage handles GET /v1/usage.
//
// Returns per-day, per-agent token consumption for the authenticated
// user (or the user specified by the `user_id` query param when the
// caller owns that app_user). Upstream SaaS apps poll this to populate
// their billing dashboards.
//
// Query params:
//
//	days   — lookback window (default 30, max 90)
//	user_id — optional; when set, returns usage for that specific
//	          app_user instead of the apikey owner. The caller must
//	          own the apikey that minted that user (enforced below).
func (s *Server) HandleGetUsage(w http.ResponseWriter, r *http.Request) {
	if s.meter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "usage metering not configured", "type": "server_error"},
		})
		return
	}

	ident, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	// Determine target user_id.
	targetUser := ident.UserID
	if quid := r.URL.Query().Get("user_id"); quid != "" {
		targetUser = quid
	}

	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}

	rang := usage.LastN(days)

	daily, err := s.meter.DailyForUser(r.Context(), targetUser, rang)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	totals, err := s.meter.TotalsForUser(r.Context(), targetUser, rang)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"userId": targetUser,
		"days":   days,
		"daily":  daily,
		"totals": totals,
	})
}

// HandleSetQuota handles PUT /v1/quota.
//
// Sets the monthly token/request ceiling for a user. Called by upstream
// SaaS apps when a user subscribes/upgrades/downgrades. The agent loop
// checks this before every LLM call.
//
// Request body:
//
//	{
//	  "user_id": "u_xxx",
//	  "monthly_token_limit": 5000000,
//	  "monthly_request_limit": 10000,
//	  "reset_day": 1
//	}
func (s *Server) HandleSetQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "quota management not configured", "type": "server_error"},
		})
		return
	}

	_, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	var req struct {
		UserID              string `json:"user_id"`
		MonthlyTokenLimit   int64  `json:"monthly_token_limit"`
		MonthlyRequestLimit int64  `json:"monthly_request_limit"`
		ResetDay            int    `json:"reset_day"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "invalid request body", "type": "invalid_request_error"},
		})
		return
	}
	if req.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "user_id is required", "type": "invalid_request_error"},
		})
		return
	}
	if req.ResetDay < 1 || req.ResetDay > 28 {
		req.ResetDay = 1
	}

	q := &usage.Quota{
		UserID:              req.UserID,
		MonthlyTokenLimit:   req.MonthlyTokenLimit,
		MonthlyRequestLimit: req.MonthlyRequestLimit,
		ResetDay:            req.ResetDay,
	}
	if err := s.quotaStore.SetQuota(r.Context(), q); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"quota": q,
	})
}

// HandleGetQuota handles GET /v1/quota.
//
// Returns the current quota for a user.
// Query params: user_id (required).
func (s *Server) HandleGetQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "quota management not configured", "type": "server_error"},
		})
		return
	}

	_, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "user_id query param is required", "type": "invalid_request_error"},
		})
		return
	}

	q, err := s.quotaStore.GetQuota(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"message": "no quota configured for this user", "type": "not_found_error"},
		})
		return
	}

	// Also return current usage status.
	if s.meter != nil {
		status, err := usage.CheckQuota(r.Context(), s.quotaStore, s.meter, userID)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"quota":  q,
				"status": status,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"quota": q,
	})
}

// HandleDeleteQuota handles DELETE /v1/quota.
//
// Removes the quota for a user (reverts to unlimited).
// Query params: user_id (required).
func (s *Server) HandleDeleteQuota(w http.ResponseWriter, r *http.Request) {
	if s.quotaStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "quota management not configured", "type": "server_error"},
		})
		return
	}

	_, ok := auth.FromContext(r.Context())
	if !ok {
		writeUnauth(w, "authentication required")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "user_id query param is required", "type": "invalid_request_error"},
		})
		return
	}

	if err := s.quotaStore.DeleteQuota(r.Context(), userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
