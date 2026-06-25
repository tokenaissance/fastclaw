package setup

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/push"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

type savePushDeviceReq struct {
	Token       string `json:"token"`
	Platform    string `json:"platform"`
	Environment string `json:"environment"`
	BundleID    string `json:"bundleId"`
}

func (s *Server) handleSavePushDevice(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not configured"})
		return
	}
	var req savePushDeviceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "token required"})
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = "ios"
	}
	if platform != "ios" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported platform"})
		return
	}
	rec := &store.PushDeviceRecord{
		UserID:      ident.EffectiveUserID(),
		Token:       token,
		Platform:    platform,
		Environment: strings.ToLower(strings.TrimSpace(req.Environment)),
		BundleID:    strings.TrimSpace(req.BundleID),
	}
	if err := s.dataStore.SavePushDevice(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeletePushDevice(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not configured"})
		return
	}
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "token required"})
		return
	}
	if err := s.dataStore.DeletePushDevice(r.Context(), ident.EffectiveUserID(), token); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWebPushOutbound(msg bus.OutboundMessage) {
	if s.pushClient == nil || !s.pushClient.Enabled() || s.dataStore == nil {
		return
	}
	if msg.Channel != "web" || strings.TrimSpace(msg.AgentID) == "" || strings.TrimSpace(msg.ChatID) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	userID, err := s.dataStore.LookupSessionOwner(ctx, msg.AgentID, msg.ChatID)
	if err != nil || userID == "" {
		slog.Debug("push: session owner not found", "agent", msg.AgentID, "session", msg.ChatID, "error", err)
		return
	}
	devices, err := s.dataStore.ListPushDevices(ctx, userID)
	if err != nil {
		slog.Warn("push: list devices failed", "user", userID, "error", err)
		return
	}
	if len(devices) == 0 {
		return
	}

	title := "FastAgent"
	if rec, err := s.dataStore.GetAgent(ctx, msg.AgentID); err == nil && rec != nil && strings.TrimSpace(rec.Name) != "" {
		title = strings.TrimSpace(rec.Name)
	}
	for _, d := range devices {
		if d.Platform != "" && d.Platform != "ios" {
			continue
		}
		if err := s.pushClient.Send(ctx, push.Notification{
			DeviceToken: d.Token,
			Title:       title,
			Body:        "你有一条新的 Agent 消息",
			AgentID:     msg.AgentID,
			SessionID:   msg.ChatID,
		}); err != nil {
			slog.Warn("push: APNs send failed", "user", userID, "error", err)
		}
	}
}
