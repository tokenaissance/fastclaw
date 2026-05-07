package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Per-agent IM channel CRUD. Wraps the existing scope.SaveChannel +
// bindings setting so the dashboard can present "connect Telegram" as
// one click instead of asking users to wire two separate config rows
// by hand.

// channelOut is the wire shape returned by GET /api/agents/<id>/channels.
// One row per (channelType, accountID); botToken is masked.
//
// Source distinguishes where this binding lives:
//   - "agent" — the agent's "official" channel (visible to every user
//     with read access; only the agent owner / admin can mutate)
//   - "user"  — the caller's own per-user overlay on this agent
//     (only the caller sees + can mutate it)
type channelOut struct {
	Type        string `json:"type"`
	AccountID   string `json:"accountId"`
	BotUsername string `json:"botUsername,omitempty"`
	BotToken    string `json:"botToken"` // masked
	Enabled     bool   `json:"enabled"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Source      string `json:"source,omitempty"`
}

// resolveChannelBindingScope decides where a connect / disconnect call
// should write its channel + binding rows. Two paths:
//
//   - Owner of agent (or platform admin): writes at scope=agent, so the
//     row is the agent's "official" channel that any user without their
//     own overlay sees. Same shape as before — single bot per (agent,
//     channelType).
//   - Non-owner with read access (public agent / apikey ACL grant):
//     writes at scope=user, scoped to the caller's user_id. The binding
//     entry still records AgentID=<this agent>, so inbound routing knows
//     which (foreign) agent to load into the caller's space. Each user
//     can bind their own bot to the same public agent without colliding
//     with anyone else's overlay.
//
// Writes 4xx and returns ok=false on permission/lookup failures.
func (s *Server) resolveChannelBindingScope(w http.ResponseWriter, r *http.Request, agentID string) (string, string, bool) {
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agent id required"})
		return "", "", false
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return "", "", false
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return "", "", false
	}
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || (ident.AuthMethod != "" && ident.CanAdminPlatform()) {
		return scope.Agent, agentID, true
	}
	// Non-owner: must be able to at least read the agent.
	if (ident.AuthMethod == "apikey" && ident.CanAccessAgent(agentID)) || rec.IsPublic {
		return scope.User, uid, true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
	return "", "", false
}

// ownsAgent gates channel-management calls. Returns (callerUID, true)
// when the caller is the agent owner OR a platform admin (super_admin
// session, type=admin apikey). Bindings/channel rows are agent-keyed so
// the returned uid is the caller's, not the owner's — that matters for
// per-caller flows like the WeChat QR session whose poll-side equality
// check needs to match the start-side that stored it.
func (s *Server) ownsAgent(r *http.Request, agentID string) (string, bool) {
	if agentID == "" {
		return "", false
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		return "", false
	}
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return "", false
	}
	if rec.UserID == uid {
		return uid, true
	}
	if ident, ok := auth.FromContext(r.Context()); ok && ident.CanAdminPlatform() {
		return uid, true
	}
	return "", false
}

func (s *Server) handleListAgentChannels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	caller := s.effectiveUserID(r)

	// Always include the agent's "official" channels (scope=agent).
	// Then overlay the caller's own per-user binding rows (scope=user)
	// — but only the entries that point at THIS agent (a user-scope
	// bindings list can span multiple foreign agents). User rows are
	// suppressed when caller IS the owner because owner writes already
	// land at agent scope; they'd otherwise get a confusing "two cards
	// for the same bot" view if they had previously self-bound.
	out := make([]channelOut, 0)
	if rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, scope.Agent, id); err == nil {
		out = append(out, flattenChannelRows(rows, "agent", "", "")...)
	}
	if caller != "" && caller != rec.UserID {
		userRows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, scope.User, caller)
		if err == nil {
			// For user-scope, we have to filter to only show channels
			// the caller has bound TO this agent. The channel row
			// itself doesn't carry an agent_id — that link lives in the
			// caller's bindings list. Build the (channel, accountID)
			// allowlist from bindings whose AgentID == this agent.
			allow := map[[2]string]bool{}
			if bindings, _ := s.loadBindings(r, scope.User, caller); bindings != nil {
				for _, b := range bindings {
					if b.AgentID == id {
						allow[[2]string{b.Match.Channel, b.Match.AccountID}] = true
					}
				}
			}
			out = append(out, flattenChannelRows(userRows, "user", "", "", filterAccounts(allow))...)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"channels": out})
}

// flattenChannelRows expands one config row per row into one channelOut
// per (channelType, accountID). source stamps where the row came from
// for the UI to render the badge. The (botToken, _, _) trio is unused
// at present — kept variadic-style so a future caller can pre-mask
// without an extra arg.
type accountFilter func(channelType, accountID string) bool

func filterAccounts(allow map[[2]string]bool) accountFilter {
	return func(channelType, accountID string) bool {
		return allow[[2]string{channelType, accountID}]
	}
}

func flattenChannelRows(rows []store.ConfigRecord, source string, _, _ string, filters ...accountFilter) []channelOut {
	out := make([]channelOut, 0, len(rows))
	for _, rec := range rows {
		cc := decodeChannelConfigFromRecord(&rec)
		if len(cc.Accounts) == 0 {
			if len(filters) > 0 && !filters[0](rec.Name, "") {
				continue
			}
			out = append(out, channelOut{
				Type:      rec.Name,
				AccountID: "",
				BotToken:  maskAPIKey(cc.BotToken),
				Enabled:   rec.Enabled,
				UpdatedAt: rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
				Source:    source,
			})
			continue
		}
		for accountID, acct := range cc.Accounts {
			if len(filters) > 0 && !filters[0](rec.Name, accountID) {
				continue
			}
			tok := acct.BotToken
			if tok == "" {
				tok = cc.BotToken
			}
			out = append(out, channelOut{
				Type:        rec.Name,
				AccountID:   accountID,
				BotUsername: accountID,
				BotToken:    maskAPIKey(tok),
				Enabled:     rec.Enabled,
				UpdatedAt:   rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
				Source:      source,
			})
		}
	}
	return out
}

type connectTelegramRequest struct {
	BotToken string `json:"botToken"`
}

// handleConnectAgentTelegram validates the bot token by hitting
// Telegram's getMe, then persists a kind=channel + binding pair scoped
// to this agent and hot-starts the adapter so the bot starts polling
// immediately.
func (s *Server) handleConnectAgentTelegram(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectTelegramRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken required"})
		return
	}

	// Validate via Telegram getMe; this also gives us the bot username
	// which we use as the binding accountID.
	username, err := telegramGetMe(token)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Build channel config: one Account keyed by bot username so multi-
	// bot setups are supported on the same agent if a user adds another
	// bot later. Per-account BotToken so each can have its own.
	cc := config.ChannelConfig{
		Enabled:  true,
		Accounts: map[string]config.AccountConfig{username: {BotToken: token}},
	}
	// credential_key MUST equal the value the Telegram adapter ships as
	// InboundMessage.AccountID — that's the column processInbound uses
	// to find the owning user (LookupChannelByCredential). The adapter
	// is created with accountID = the Accounts-map key, which is the
	// bot's @username, so we mirror that here. Using the token-tail
	// fallback (credentialKeyFor) silently dropped every inbound
	// message because no row matched.
	credKey := username
	if err := s.assertChannelCredentialUnique(r, "telegram", credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "telegram", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Append a binding so inbound messages route to this agent. Existing
	// bindings (e.g. an earlier Discord bot) are preserved. AgentID
	// inside the entry is always the path-resolved agent (= scopeID
	// when sc=Agent; the foreign agent the caller is binding to when
	// sc=User).
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "telegram", AccountID: username},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "telegram", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"botUsername": username,
	})
}

func (s *Server) handleDisconnectAgentChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	channelType := r.PathValue("type")
	accountID := r.PathValue("accountId")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	// Locate the channel row at the resolved scope (agent for owner /
	// admin, user for non-owner overlay). Match by accountID inside the
	// row's Accounts map.
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindChannel, sc, scopeID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, rec := range rows {
		if rec.Name != channelType {
			continue
		}
		cc := decodeChannelConfigFromRecord(&rec)
		_, hasAcct := cc.Accounts[accountID]
		// When the row has no Accounts map, treat it as the legacy
		// single-bot shape; accountID must be empty to match.
		if !hasAcct && !(len(cc.Accounts) == 0 && accountID == "") {
			continue
		}
		if hasAcct {
			delete(cc.Accounts, accountID)
		}
		// If nothing left, drop the row; otherwise rewrite it.
		if len(cc.Accounts) == 0 && (cc.BotToken == "" || hasAcct) {
			if err := s.dataStore.DeleteConfig(r.Context(), rec.ID); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else {
			if err := scope.SaveChannel(r.Context(), s.dataStore, rec.Scope, rec.ScopeID, rec.Name, rec.CredentialKey, rec.Enabled, cc); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		// Drop the matching binding too.
		if err := s.removeBinding(r, sc, scopeID, id, channelType, accountID); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.invalidateScope(sc, scopeID)
		s.hotUnregisterChannel(channelType, accountID)
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	jsonResponse(w, http.StatusNotFound, map[string]any{"error": "binding not found"})
}

// appendBinding loads the (scope, scopeID) `bindings` setting, appends
// the new binding (deduped on (agentID, channel, accountID) — agent
// scope rows always carry one agent so AgentID is redundant there, but
// user-scope rows can hold bindings to multiple agents), and saves it
// back.
func (s *Server) appendBinding(r *http.Request, sc, scopeID string, b config.Binding) error {
	cur, err := s.loadBindings(r, sc, scopeID)
	if err != nil {
		return err
	}
	for _, existing := range cur {
		if existing.AgentID == b.AgentID &&
			existing.Match.Channel == b.Match.Channel &&
			existing.Match.AccountID == b.Match.AccountID {
			return nil // already present
		}
	}
	next := append(cur, b)
	return s.saveBindings(r, sc, scopeID, next)
}

// removeBinding strips the (agentID, channelType, accountID) entry from
// the (scope, scopeID) bindings list. agentID matters at scope=user
// where one row holds bindings spanning multiple foreign agents — at
// scope=agent it's the same value as scopeID, but the equality check
// is harmless either way.
func (s *Server) removeBinding(r *http.Request, sc, scopeID, agentID, channelType, accountID string) error {
	cur, err := s.loadBindings(r, sc, scopeID)
	if err != nil {
		return err
	}
	out := make([]config.Binding, 0, len(cur))
	for _, b := range cur {
		if b.AgentID == agentID && b.Match.Channel == channelType && b.Match.AccountID == accountID {
			continue
		}
		out = append(out, b)
	}
	return s.saveBindings(r, sc, scopeID, out)
}

func (s *Server) loadBindings(r *http.Request, sc, scopeID string) ([]config.Binding, error) {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, sc, scopeID, "bindings")
	if err != nil || rec == nil {
		return nil, nil
	}
	// stored shape is {"list": [...]}; tolerate raw arrays for
	// hand-edited rows.
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		var wrap struct {
			List []config.Binding `json:"list"`
		}
		if uerr := json.Unmarshal(blob, &wrap); uerr == nil && wrap.List != nil {
			return wrap.List, nil
		}
		var raw []config.Binding
		if uerr := json.Unmarshal(blob, &raw); uerr == nil {
			return raw, nil
		}
	}
	return nil, nil
}

func (s *Server) saveBindings(r *http.Request, sc, scopeID string, list []config.Binding) error {
	if len(list) == 0 {
		// Empty list — delete the row to keep the namespace clean.
		// Best-effort; not-found is fine.
		if rec, _ := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, sc, scopeID, "bindings"); rec != nil {
			_ = s.dataStore.DeleteConfig(r.Context(), rec.ID)
		}
		return nil
	}
	wrap := map[string]interface{}{"list": list}
	return scope.SaveSetting(r.Context(), s.dataStore, sc, scopeID, "bindings", wrap)
}

// telegramGetMe validates the bot token by hitting the Bot API. Returns
// the bot's username on success. We avoid pulling tgbotapi here so this
// handler doesn't drag in the full long-poll bot machinery for what's
// just a HEAD-style validation.
func telegramGetMe(token string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("contact telegram: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Telegram returns {"ok":false,"description":"..."} on bad tokens.
		var apiErr struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Description != "" {
			return "", fmt.Errorf("telegram rejected token: %s", apiErr.Description)
		}
		return "", fmt.Errorf("telegram getMe: HTTP %d", resp.StatusCode)
	}
	var ok struct {
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return "", fmt.Errorf("parse telegram response: %w", err)
	}
	if ok.Result.Username == "" {
		return "", errors.New("telegram getMe returned empty username")
	}
	return ok.Result.Username, nil
}

// --- Discord ---

type connectDiscordRequest struct {
	BotToken string `json:"botToken"`
}

// handleConnectAgentDiscord validates a Discord bot token by calling
// /users/@me on the Discord REST API, then persists kind=channel +
// binding rows just like the Telegram flow. accountID = bot user ID
// (Discord's stable identifier, unlike username which can be changed).
func (s *Server) handleConnectAgentDiscord(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectDiscordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken required"})
		return
	}

	userID, username, err := discordGetMe(token)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// accountID = Discord user ID. Stable across username changes
	// and matches what the Discord adapter ships in
	// InboundMessage.AccountID (it's set from the same value).
	cc := config.ChannelConfig{
		Enabled:  true,
		Accounts: map[string]config.AccountConfig{userID: {BotToken: token}},
	}
	credKey := userID
	if err := s.assertChannelCredentialUnique(r, "discord", credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "discord", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "discord", AccountID: userID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "discord", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"botUsername": username,
		"botUserId":   userID,
	})
}

// discordGetMe validates the bot token via the Discord REST API.
// Endpoint docs: GET /users/@me with `Authorization: Bot <token>`
// returns the bot user object (id, username, discriminator). We avoid
// pulling discordgo here so this handler doesn't open a gateway
// connection just to check a token.
func discordGetMe(token string) (string, string, error) {
	req, err := http.NewRequest("GET", "https://discord.com/api/v10/users/@me", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bot "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("contact discord: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Discord returns {"message": "...", "code": ...} on auth errors.
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			return "", "", fmt.Errorf("discord rejected token: %s", apiErr.Message)
		}
		return "", "", fmt.Errorf("discord users/@me: HTTP %d", resp.StatusCode)
	}
	var me struct {
		ID            string `json:"id"`
		Username      string `json:"username"`
		Discriminator string `json:"discriminator"`
		Bot           bool   `json:"bot"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		return "", "", fmt.Errorf("parse discord response: %w", err)
	}
	if me.ID == "" {
		return "", "", errors.New("discord users/@me returned empty id")
	}
	if !me.Bot {
		return "", "", errors.New("token belongs to a user account, not a bot — connect a bot token from the Discord Developer Portal")
	}
	display := me.Username
	if me.Discriminator != "" && me.Discriminator != "0" {
		// Legacy Discord usernames are user#1234. Modern (post-2023)
		// accounts have discriminator "0" — display just the handle.
		display = me.Username + "#" + me.Discriminator
	}
	return me.ID, display, nil
}

// --- Slack ---

type connectSlackRequest struct {
	BotToken string `json:"botToken"`
	AppToken string `json:"appToken"`
}

// handleConnectAgentSlack persists the Slack bot+app token pair after
// validating via auth.test. Slack needs both: bot token (xoxb-...)
// for posting/reading, app token (xapp-...) for Socket Mode WS.
// accountID = team_id so a workspace's events all route to the same
// agent (per-workspace Slack apps are the common shape).
func (s *Server) handleConnectAgentSlack(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectSlackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	botToken := strings.TrimSpace(req.BotToken)
	appToken := strings.TrimSpace(req.AppToken)
	if botToken == "" || appToken == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken and appToken both required"})
		return
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "botToken should start with xoxb-"})
		return
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appToken should start with xapp- (app-level token from Settings → Basic Information)"})
		return
	}

	teamID, teamName, botUserID, err := slackAuthTest(botToken)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Slack channel rows put both tokens in top-level BotToken/AppToken
	// (the Slack adapter constructor reads them as a pair). Accounts
	// map keyed by team_id so the inbound side resolves owner via
	// LookupChannelByCredential(channel="slack", credKey=teamID).
	cc := config.ChannelConfig{
		Enabled:  true,
		BotToken: botToken,
		AppToken: appToken,
		Accounts: map[string]config.AccountConfig{teamID: {BotToken: botToken}},
	}
	credKey := teamID
	if err := s.assertChannelCredentialUnique(r, "slack", credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "slack", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "slack", AccountID: teamID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "slack", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":        true,
		"teamName":  teamName,
		"teamId":    teamID,
		"botUserId": botUserID,
	})
}

// slackAuthTest hits Slack's auth.test endpoint with the bot token to
// validate it AND capture team_id/team_name/bot_user_id in one call.
// Doc: https://api.slack.com/methods/auth.test
func slackAuthTest(botToken string) (teamID, teamName, botUserID string, err error) {
	req, rerr := http.NewRequest("POST", "https://slack.com/api/auth.test", nil)
	if rerr != nil {
		return "", "", "", rerr
	}
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		return "", "", "", fmt.Errorf("contact slack: %w", derr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Slack always returns 200 + a JSON body; the `ok` field carries
	// the actual result.
	var ok struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
	}
	if jerr := json.Unmarshal(body, &ok); jerr != nil {
		return "", "", "", fmt.Errorf("parse slack response: %w", jerr)
	}
	if !ok.OK {
		msg := ok.Error
		if msg == "" {
			msg = "unknown error"
		}
		return "", "", "", fmt.Errorf("slack rejected token: %s", msg)
	}
	if ok.TeamID == "" {
		return "", "", "", errors.New("slack auth.test returned empty team_id")
	}
	return ok.TeamID, ok.Team, ok.UserID, nil
}

// --- WeChat (iLink) ---
//
// Unlike Telegram/Discord/Slack, WeChat doesn't take a paste-it-in
// token. The user scans a QR code with the WeChat phone app; on
// confirmation iLink hands back a (bot_token, ilink_bot_id,
// ilink_user_id, baseurl) tuple. Two-step flow:
//
//   POST /api/agents/{id}/channels/wechat/login
//     → fetch a QR token from iLink, render as image on the client.
//       Returns {sessionID, qrCode, qrCodeImg}.
//
//   GET  /api/agents/{id}/channels/wechat/login/status?session=<id>
//     → poll iLink's get_qrcode_status one round-trip.
//       Returns {status: wait|scaned|confirmed|expired, connected,
//       accountId?}. On `confirmed`, persists the channel row +
//       binding and hot-registers the adapter, so the next poll the
//       client makes for sandbox/agent state shows the bot live.

const (
	wechatILinkBase    = "https://ilinkai.weixin.qq.com"
	wechatQRCodeURL    = wechatILinkBase + "/ilink/bot/get_bot_qrcode?bot_type=3"
	wechatQRStatusURL  = wechatILinkBase + "/ilink/bot/get_qrcode_status?qrcode="
	wechatStatusWait   = "wait"
	wechatStatusScaned = "scaned"
	wechatStatusOK     = "confirmed"
	wechatStatusExpire = "expired"
)

// wechatLoginSession tracks an in-flight QR scan. Lives in memory only
// — abandoned sessions get GC'd via the TTL sweep on the registry.
// Saving to the store would let polls survive process restart but the
// QR token itself expires in iLink server-side after a couple of
// minutes anyway, so cross-restart resumption isn't worth the
// complexity.
type wechatLoginSession struct {
	qrCode    string // iLink token, used both as polling key + as QR payload
	qrCodeImg string // optional pre-rendered QR image (base64 or URL)
	agentID   string // which agent the credentials should bind to
	userID    string // initiating caller — verified on every status poll
	scope     string // resolved storage scope ("agent" for owner/admin,
	// "user" for non-owner overlay) — captured at start so persist on
	// confirm uses the same scope the caller initially saw.
	scopeID   string
	createdAt time.Time
}

type wechatLoginRegistry struct {
	mu       sync.Mutex
	sessions map[string]*wechatLoginSession
}

var wechatLogins = &wechatLoginRegistry{sessions: map[string]*wechatLoginSession{}}

func (r *wechatLoginRegistry) put(id string, s *wechatLoginSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[id] = s
	// Opportunistic GC: drop sessions older than 5 minutes (QR codes
	// expire well before this server-side; anything older is dead).
	cutoff := time.Now().Add(-5 * time.Minute)
	for k, v := range r.sessions {
		if v.createdAt.Before(cutoff) {
			delete(r.sessions, k)
		}
	}
}

func (r *wechatLoginRegistry) get(id string) *wechatLoginSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

func (r *wechatLoginRegistry) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

// handleStartAgentWeChatLogin asks iLink for a fresh QR code, registers
// a server-side session keyed by the returned qrCode token, and hands
// the client back what it needs to render the QR image. The actual
// scan happens out-of-band in the user's WeChat phone app; the client
// then polls handleAgentWeChatLoginStatus to drive the state machine.
func (s *Server) handleStartAgentWeChatLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}
	uid := s.effectiveUserID(r)

	qr, err := wechatFetchQRCode(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	sessionID := qr.QRCode // iLink's token is unique enough; reuse it
	wechatLogins.put(sessionID, &wechatLoginSession{
		qrCode:    qr.QRCode,
		qrCodeImg: qr.QRCodeImgContent,
		agentID:   id,
		userID:    uid,
		scope:     sc,
		scopeID:   scopeID,
		createdAt: time.Now(),
	})
	jsonResponse(w, http.StatusOK, map[string]any{
		"sessionId": sessionID,
		"qrCode":    qr.QRCode,
		"qrCodeImg": qr.QRCodeImgContent,
	})
}

// handleAgentWeChatLoginStatus polls iLink for the current scan state
// of this session's QR code. On `confirmed`, persists the channel row
// + binding + hot-registers the adapter — same shape as the Telegram /
// Discord / Slack connect handlers, but driven by the QR status
// machine instead of an immediate token validation.
func (s *Server) handleAgentWeChatLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.resolveChannelBindingScope(w, r, id); !ok {
		return
	}
	uid := s.effectiveUserID(r)
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "session required"})
		return
	}
	sess := wechatLogins.get(sessionID)
	if sess == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found or expired"})
		return
	}
	// Cross-tenant guard: don't let one user's poll observe another
	// user's QR session even with a guessed sessionID.
	if sess.userID != uid || sess.agentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}

	status, err := wechatPollQRStatus(r.Context(), sess.qrCode)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	switch status.Status {
	case wechatStatusOK:
		// User confirmed on phone. iLink returned credentials; persist
		// + bind + hot-register, then drop the in-flight session.
		creds := wechatCredentials{
			BotToken:    status.BotToken,
			ILinkBotID:  status.ILinkBotID,
			BaseURL:     status.BaseURL,
			ILinkUserID: status.ILinkUserID,
		}
		if creds.BotToken == "" || creds.ILinkBotID == "" {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "ilink confirmed without credentials"})
			return
		}
		if err := s.persistWeChatAccount(r, sess.scope, sess.scopeID, id, creds); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		wechatLogins.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "confirmed",
			"connected": true,
			"accountId": creds.ILinkBotID,
		})
		return
	case wechatStatusExpire:
		wechatLogins.delete(sessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    "expired",
			"connected": false,
		})
		return
	default:
		// wait / scaned / unknown — keep polling. We surface "scaned"
		// distinctly because the UI flips to "扫描完成,请确认" when the
		// user has tapped the QR but not yet pressed confirm on phone.
		jsonResponse(w, http.StatusOK, map[string]any{
			"status":    status.Status,
			"connected": false,
		})
		return
	}
}

// persistWeChatAccount writes a kind=channel row + binding for a
// freshly-confirmed iLink account, identical in shape to what
// handleConnectAgentTelegram does for a token paste — just sourcing
// the credentials from QR confirmation instead of user input.
func (s *Server) persistWeChatAccount(r *http.Request, sc, scopeID, agentID string, creds wechatCredentials) error {
	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			creds.ILinkBotID: {
				BotToken: creds.BotToken,
				BaseURL:  creds.BaseURL,
				UserID:   creds.ILinkUserID,
			},
		},
	}
	credKey := creds.ILinkBotID
	if err := s.assertChannelCredentialUnique(r, "wechat", credKey, ""); err != nil {
		return err
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "wechat", credKey, true, cc); err != nil {
		return err
	}
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: agentID,
		Match:   config.Match{Channel: "wechat", AccountID: creds.ILinkBotID},
	}); err != nil {
		return err
	}
	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "wechat", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	return nil
}

// --- iLink HTTP helpers (validation-only; running adapter has its own
//     client in internal/channels/wechat.go) ---

type wechatCredentials struct {
	BotToken    string
	ILinkBotID  string
	BaseURL     string
	ILinkUserID string
}

type wechatQRCodeResp struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type wechatQRStatusResp struct {
	Status      string `json:"status"`
	BotToken    string `json:"bot_token"`
	ILinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}

func wechatFetchQRCode(ctx context.Context) (*wechatQRCodeResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wechatQRCodeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact ilink: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink qrcode HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out wechatQRCodeResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse ilink qrcode: %w", err)
	}
	if out.QRCode == "" {
		return nil, errors.New("ilink returned empty qrcode")
	}
	return &out, nil
}

// wechatPollQRStatus does ONE round-trip — returns whatever the server
// says right now. We don't long-poll on the server side because the
// upstream endpoint already does (~40s); doing it on every status
// request would mean a tab refresh stalls 40s. The client polls every
// 3s instead, mirroring the workany-web shape.
func wechatPollQRStatus(ctx context.Context, qrcode string) (*wechatQRStatusResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wechatQRStatusURL+qrcode, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact ilink: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ilink status HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out wechatQRStatusResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse ilink status: %w", err)
	}
	return &out, nil
}

// --- Feishu / Feishu ---

type connectFeishuRequest struct {
	AppID             string `json:"appId"`
	AppSecret         string `json:"appSecret"`
	VerificationToken string `json:"verificationToken"`
	EncryptKey        string `json:"encryptKey"`
	UseLongConn       bool   `json:"useLongConn"`
}

// handleConnectAgentFeishu validates a Feishu custom-app credential triple
// by minting a tenant_access_token (proves app_id+app_secret are
// valid) and fetching /bot/v3/info (captures the bot's display name).
// Stores the triple as kind=channel + binding rows + hot-registers
// the adapter.
//
// Storage layout mirrors slack/wechat: credKey = app_id (also the
// accountID), AccountConfig.BotToken = app_secret, AccountConfig.UserID
// = verification_token (matches the field's "extra account-scoped
// identifier" comment).
func (s *Server) handleConnectAgentFeishu(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectFeishuRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	appID := strings.TrimSpace(req.AppID)
	appSecret := strings.TrimSpace(req.AppSecret)
	verificationToken := strings.TrimSpace(req.VerificationToken)
	encryptKey := strings.TrimSpace(req.EncryptKey)
	useLongConn := req.UseLongConn
	if appID == "" || appSecret == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appId and appSecret required"})
		return
	}
	if !strings.HasPrefix(appID, "cli_") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appId should start with cli_ (Feishu custom-app App ID)"})
		return
	}

	botName, botOpenID, err := channels.FeishuValidateCredentials(r.Context(), appID, appSecret)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			appID: {
				BotToken:    appSecret,
				UserID:      verificationToken, // see channels/feishu.go field-mapping note
				EncryptKey:  encryptKey,
				UseLongConn: useLongConn,
			},
		},
	}
	credKey := appID
	if err := s.assertChannelCredentialUnique(r, "feishu", credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "feishu", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "feishu", AccountID: appID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "feishu", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	resp := map[string]any{
		"ok":          true,
		"appId":       appID,
		"botName":     botName,
		"botOpenId":   botOpenID,
		"useLongConn": useLongConn,
	}
	// Webhook URL is only meaningful when the user picked the
	// public-URL transport. Long-connection accounts don't need it
	// (no public ingress required) — omit so the UI doesn't show a
	// step the user can't / shouldn't do.
	if !useLongConn {
		resp["webhookUrl"] = feishuWebhookPathFor(r, appID)
	}
	jsonResponse(w, http.StatusOK, resp)
}

// feishuWebhookPathFor builds the URL the user should paste into the
// Feishu Developer Console's "Event Subscriptions → Request URL" field.
// Best-effort — uses the request's Host so reverse-proxied deployments
// surface the user-facing hostname rather than the bind address.
func feishuWebhookPathFor(r *http.Request, appID string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/api/feishu/webhook/" + appID
}

// --- LINE ---

type connectLINERequest struct {
	ChannelToken  string `json:"channelToken"`
	ChannelSecret string `json:"channelSecret"`
}

// handleConnectAgentLINE validates a LINE Messaging API channel by
// hitting /v2/bot/info with the channel access token. Captures the
// bot's userId (used as accountID) + display name + basicId. Stores
// channel_access_token in AccountConfig.BotToken, channel_secret in
// AccountConfig.UserID (matching the field-mapping convention used by
// the WeChat / Feishu adapters).
//
// channel_secret is technically optional — the adapter can run without
// signature validation — but webhook traffic flows over the open
// internet so we strongly recommend setting it. The connect handler
// accepts an empty string and warns at validation time only if the
// secret is missing.
func (s *Server) handleConnectAgentLINE(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	sc, scopeID, ok := s.resolveChannelBindingScope(w, r, id)
	if !ok {
		return
	}

	var req connectLINERequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	channelToken := strings.TrimSpace(req.ChannelToken)
	channelSecret := strings.TrimSpace(req.ChannelSecret)
	if channelToken == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "channelToken required"})
		return
	}

	userID, displayName, basicID, err := channels.LINEValidateCredentials(r.Context(), channelToken)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	cc := config.ChannelConfig{
		Enabled: true,
		Accounts: map[string]config.AccountConfig{
			userID: {
				BotToken: channelToken,
				UserID:   channelSecret,
			},
		},
	}
	credKey := userID
	if err := s.assertChannelCredentialUnique(r, "line", credKey, ""); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if err := scope.SaveChannel(r.Context(), s.dataStore, sc, scopeID, "line", credKey, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendBinding(r, sc, scopeID, config.Binding{
		AgentID: id,
		Match:   config.Match{Channel: "line", AccountID: userID},
	}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "line", credKey); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"botUserId":   userID,
		"botName":     displayName,
		"basicId":     basicID,
		"webhookUrl":  lineWebhookPathFor(r, userID),
	})
}

// lineWebhookPathFor returns the URL the user pastes into LINE
// Developers Console under "Messaging API → Webhook URL". Same shape
// as feishuWebhookPathFor — surfaces the public-facing host via the
// usual reverse-proxy headers.
func lineWebhookPathFor(r *http.Request, userID string) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + "/api/line/webhook/" + userID
}
