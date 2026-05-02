package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/session"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

type agentChatEvent = agent.ChatEvent

// loadUserConfig reads the merged Config view for the request's user.
// Walks system → user setting namespaces + the scope-aware provider /
// channel rows. The result is the same shape gateway.assembleConfig
// produces — UI-only fields like Storage/Gateway are filled by env
// overlay, not by the DB.
func (s *Server) loadUserConfig(r *http.Request) (*config.Config, error) {
	if s.dataStore == nil {
		return &config.Config{}, nil
	}
	uid := config.UserIDFromContext(r.Context())
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Channels:  map[string]config.ChannelConfig{},
	}
	for _, ns := range settingNamespaces {
		if err := scope.SettingInto(r.Context(), s.dataStore, ns.namespace, uid, "", "", ns.dst(cfg)); err != nil {
			return nil, err
		}
	}
	if provs, err := scope.Providers(r.Context(), s.dataStore, uid, ""); err == nil {
		for k, v := range provs {
			cfg.Providers[k] = v
		}
	}
	if chs, err := scope.Channels(r.Context(), s.dataStore, uid, ""); err == nil {
		for k, v := range chs {
			cfg.Channels[k] = v
		}
	}
	if ae, err := loadAgentSkillEntriesForUser(r.Context(), s.dataStore, uid); err == nil && len(ae) > 0 {
		cfg.Skills.AgentEntries = ae
	}
	config.LoadEnv().ApplyToConfig(cfg)
	config.ApplyDefaults(cfg)
	return cfg, nil
}

// loadAgentSkillEntriesForUser collects every agent-scope skills.entries
// row owned by this user. Replaces the legacy single-row keyed-by-agent
// blob — each agent now persists its own row, so the JSON we hand back
// is rebuilt by listing the user's agents and pulling each one's row.
func loadAgentSkillEntriesForUser(ctx context.Context, st store.Store, userID string) (map[string]map[string]config.SkillEntryCfg, error) {
	if st == nil || userID == "" {
		return nil, nil
	}
	agents, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]config.SkillEntryCfg{}
	for _, ar := range agents {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, store.ScopeAgent, ar.ID, "skills.entries")
		if err != nil || rec == nil || len(rec.Data) == 0 {
			continue
		}
		blob, _ := json.Marshal(rec.Data)
		var entries map[string]config.SkillEntryCfg
		if json.Unmarshal(blob, &entries) == nil && len(entries) > 0 {
			out[ar.ID] = entries
		}
	}
	return out, nil
}

// saveAgentSkillEntries upserts the agent-scope skills.entries row.
// Empty inner map deletes the row (no overrides → no row, keeps the
// configs table tight). Authorization is the caller's responsibility;
// we just persist what was requested.
func saveAgentSkillEntries(ctx context.Context, st store.Store, agentID string, entries map[string]config.SkillEntryCfg) error {
	if len(entries) == 0 {
		return scope.SaveSetting(ctx, st, scope.Agent, agentID, "skills.entries", nil)
	}
	blob, _ := json.Marshal(entries)
	var asMap map[string]interface{}
	_ = json.Unmarshal(blob, &asMap)
	return scope.SaveSetting(ctx, st, scope.Agent, agentID, "skills.entries", asMap)
}

// saveUserConfig persists the namespaced setting rows for the calling
// user's scope. Providers/Channels live in their own configs rows
// and are NOT touched here — the dedicated /api/providers and /api/channels
// endpoints (and the onboard handler) write those.
func (s *Server) saveUserConfig(r *http.Request, cfg *config.Config) error {
	if s.dataStore == nil {
		return errors.New("store not configured")
	}
	ident, ok := authIdentity(r)
	scopeName := scope.User
	scopeID := ""
	if ok && ident.Role == "super_admin" {
		// Super-admin save without ?actAs= writes to system; with
		// ?actAs=<id> writes into that user's scope (handled implicitly
		// because EffectiveUserID() returns the impersonated id).
		if !ident.IsActingAs() {
			scopeName = scope.System
		} else {
			scopeID = ident.EffectiveUserID()
		}
	} else if ok {
		scopeID = ident.UserID
	}
	for _, ns := range settingNamespaces {
		data := ns.collect(cfg)
		if err := scope.SaveSetting(r.Context(), s.dataStore, scopeName, scopeID, ns.namespace, data); err != nil {
			return err
		}
	}
	return nil
}

// settingNamespaces is the table that drives loadUserConfig /
// saveUserConfig. Adding a new sub-block of Config to the round-trip is
// a single append here.
var settingNamespaces = []settingNamespace{
	{namespace: "agents.defaults",
		dst:     func(c *config.Config) interface{} { return &c.Agents.Defaults },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Agents.Defaults) }},
	{namespace: "sandbox",
		dst:     func(c *config.Config) interface{} { return &c.Sandbox },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Sandbox) }},
	{namespace: "objectstore",
		dst:     func(c *config.Config) interface{} { return &c.ObjectStore },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.ObjectStore) }},
	{namespace: "hooks",
		dst:     func(c *config.Config) interface{} { return &c.Hooks },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Hooks) }},
	{namespace: "plugins",
		dst:     func(c *config.Config) interface{} { return &c.Plugins },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Plugins) }},
	{namespace: "taskqueue",
		dst:     func(c *config.Config) interface{} { return &c.TaskQueue },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.TaskQueue) }},
	{namespace: "tools.providers",
		dst:     func(c *config.Config) interface{} { return &c.ToolProviders },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.ToolProviders) }},
	{namespace: "tools.categories",
		dst:     func(c *config.Config) interface{} { return &c.Tools },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Tools) }},
	{namespace: "skills.install",
		dst:     func(c *config.Config) interface{} { return &c.Skills.Install },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Skills.Install) }},
	{namespace: "skills.entries",
		dst:     func(c *config.Config) interface{} { return &c.Skills.Entries },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Skills.Entries) }},
	// Per-agent skill env/key overrides have been split off this table
	// into one row per agent at scope=agent, name=skills.entries — see
	// loadAgentSkillEntriesForUser / saveAgentSkillEntries below. Lumping
	// every agent's overrides into a single user/system-scope row let
	// the JSON blob grow with every agent × skill, and forced a full
	// rewrite on every patch.
	{namespace: "memory",
		dst:     func(c *config.Config) interface{} { return &c.Memory },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Memory) }},
	{namespace: "privacy",
		dst:     func(c *config.Config) interface{} { return &c.Privacy },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Privacy) }},
	{namespace: "skillsLearner",
		dst:     func(c *config.Config) interface{} { return &c.SkillsLearner },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.SkillsLearner) }},
	{namespace: "heartbeat",
		dst:     func(c *config.Config) interface{} { return &c.Heartbeat },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Heartbeat) }},
	{namespace: "teams",
		dst:     func(c *config.Config) interface{} { return &c.Teams },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Teams) }},
	{namespace: "bindings",
		dst:     func(c *config.Config) interface{} { return &c.Bindings },
		collect: func(c *config.Config) map[string]interface{} {
			if len(c.Bindings) == 0 {
				return nil
			}
			return map[string]interface{}{"list": c.Bindings}
		}},
}

type settingNamespace struct {
	namespace string
	dst       func(*config.Config) interface{}
	collect   func(*config.Config) map[string]interface{}
}

func toMap(v interface{}) map[string]interface{} {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	if len(m) == 0 {
		return nil
	}
	return m
}

// wrapKeyed marshals a map[string]X into map[string]interface{} so it
// fits the configs.data column. Empty maps return nil so
// SaveSetting deletes the row instead of writing {}.
func wrapKeyed(v interface{}) map[string]interface{} {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	if len(m) == 0 {
		return nil
	}
	return m
}

func authIdentity(r *http.Request) (auth.Identity, bool) {
	return auth.FromContext(r.Context())
}

// resolveAgent returns the AgentHandle for the given agent within the
// caller's user space. Apikey callers are additionally checked against
// their access list before the handle is returned. super_admin without
// an actAs override gets the foreign agent injected into their OWN
// UserSpace — sessions, memory, and provider scope stay caller-keyed
// (admin doesn't see the owner's chats), while the agent's persistent
// identity (system prompt, agent-scope config, skills, files — all
// keyed by agent_id) is reused.
func (s *Server) resolveAgent(r *http.Request, agentID string) AgentHandle {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return nil
	}
	if !ident.CanAccessAgent(agentID) {
		return nil
	}
	if s.userResolver == nil {
		return nil
	}
	uid := ident.EffectiveUserID()
	space, err := s.userResolver.UserSpaceFor(uid)
	if err != nil || space == nil || space.Agents == nil {
		return nil
	}
	ag := space.Agents.AgentByID(agentID)
	// Lazy-attach when the agent isn't in the caller's UserSpace but
	// the caller is otherwise authorized to use it. Three concrete
	// scenarios this covers:
	//
	//   1. super_admin browsing another user's agent (legacy case).
	//   2. api_key whose ACL grants this agent — typically the key
	//      owner == agent owner, but this path also handles the
	//      app_user case where SwitchToAppUser flipped the identity
	//      to a fresh app_user whose UserSpace has no agents at all.
	//      Sessions/files written under that UserSpace then partition
	//      per end-user, which is the desired isolation.
	//   3. (future) cross-user grants when we add agent sharing.
	//
	// CanAccessAgent above is the only authorization gate; this just
	// hydrates the in-memory Manager. EnsureAgent is idempotent.
	if ag == nil {
		injector, hasInjector := s.userResolver.(api.AgentInjector)
		canAttach := hasInjector &&
			(ident.AuthMethod == "apikey" ||
				(ident.Role == users.RoleSuperAdmin && !ident.IsActingAs()))
		if canAttach {
			if err := injector.EnsureAgent(r.Context(), uid, agentID); err == nil {
				ag = space.Agents.AgentByID(agentID)
			}
		}
	}
	if ag == nil {
		return nil
	}
	return ag
}

func (s *Server) resolveAllAgents(r *http.Request) []AgentHandle {
	ident, ok := auth.FromContext(r.Context())
	if !ok || s.userResolver == nil {
		return nil
	}
	space, err := s.userResolver.UserSpaceFor(ident.EffectiveUserID())
	if err != nil || space == nil || space.Agents == nil {
		return nil
	}
	all := space.Agents.All()
	out := make([]AgentHandle, 0, len(all))
	for _, ag := range all {
		if !ident.CanAccessAgent(ag.Name()) {
			continue
		}
		out = append(out, ag)
	}
	return out
}

// --- /api/status ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	configured := false
	if s.accounts != nil {
		if n, err := s.accounts.Count(r.Context()); err == nil && n > 0 {
			configured = true
		}
	}
	resp := map[string]any{
		"configured": configured,
		"running":    s.userResolver != nil,
		"port":       s.port,
		"agents":     []any{},
		"channels":   []any{},
		"provider":   nil,
		"uptime":     formatDuration(time.Since(s.startedAt)),
	}
	ident, authed := auth.FromContext(r.Context())
	if !authed {
		jsonResponse(w, http.StatusOK, resp)
		return
	}
	resp["userId"] = ident.UserID
	resp["role"] = ident.Role
	resp["isAdmin"] = ident.Role == "super_admin"
	if resp["isAdmin"].(bool) && s.accounts != nil {
		if n, err := s.accounts.Count(r.Context()); err == nil {
			resp["users"] = n
		}
	}

	if !configured {
		jsonResponse(w, http.StatusOK, resp)
		return
	}
	cfg, err := s.loadUserConfig(r)
	if err == nil {
		// Pick the provider that actually backs the default model. The
		// model id is "<providerName>/<modelID>" (split on first slash —
		// modelIDs themselves can contain slashes, e.g.
		// "openrouter/xiaomi/mimo-v2-flash"). Falling back to "first
		// provider in the map" produced a mismatched panel where the
		// header said one provider but the default model belonged to
		// another.
		defaultModel := cfg.Agents.Defaults.Model
		var provName string
		if i := strings.IndexByte(defaultModel, '/'); i > 0 {
			provName = defaultModel[:i]
		}
		if prov, ok := cfg.Providers[provName]; ok {
			resp["provider"] = map[string]string{
				"name":    provName,
				"model":   defaultModel,
				"apiBase": prov.APIBase,
				"apiKey":  maskAPIKey(prov.APIKey),
			}
		} else {
			for name, prov := range cfg.Providers {
				resp["provider"] = map[string]string{
					"name":    name,
					"model":   defaultModel,
					"apiBase": prov.APIBase,
					"apiKey":  maskAPIKey(prov.APIKey),
				}
				break
			}
		}
		var chs []map[string]string
		for chType, ch := range cfg.Channels {
			if !ch.Enabled {
				continue
			}
			chs = append(chs, map[string]string{"type": chType})
		}
		if len(chs) > 0 {
			resp["channels"] = chs
		}
	}
	allAgents := s.resolveAllAgents(r)
	if len(allAgents) > 0 {
		var agentList []map[string]string
		for _, ag := range allAgents {
			id := ag.Name() // AgentHandle.Name() returns the agent id
			entry := map[string]string{"id": id}
			// Surface the human-friendly name from the agents row so the
			// dashboard list reads "default" / "ImgAny" instead of
			// "agt_…". Look-up failures fall back to id-only so a
			// transient store error doesn't black out the panel.
			if s.dataStore != nil {
				if rec, _ := s.dataStore.GetAgent(r.Context(), id); rec != nil && rec.Name != "" {
					entry["name"] = rec.Name
				}
			}
			agentList = append(agentList, entry)
		}
		resp["agents"] = agentList
	}
	jsonResponse(w, http.StatusOK, resp)
}

// --- /api/config (GET / POST) ---

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	masked := *cfg
	masked.Providers = make(map[string]config.ProviderConfig)
	for k, v := range cfg.Providers {
		v.APIKey = maskAPIKey(v.APIKey)
		masked.Providers[k] = v
	}
	if len(cfg.Skills.Entries) > 0 {
		me := make(map[string]config.SkillEntryCfg, len(cfg.Skills.Entries))
		for k, v := range cfg.Skills.Entries {
			me[k] = maskSkillEntry(v)
		}
		masked.Skills.Entries = me
	}
	if len(cfg.Skills.AgentEntries) > 0 {
		ma := make(map[string]map[string]config.SkillEntryCfg, len(cfg.Skills.AgentEntries))
		for aid, inner := range cfg.Skills.AgentEntries {
			out := make(map[string]config.SkillEntryCfg, len(inner))
			for k, v := range inner {
				out[k] = maskSkillEntry(v)
			}
			ma[aid] = out
		}
		masked.Skills.AgentEntries = ma
	}
	jsonResponse(w, http.StatusOK, masked)
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	// PATCH semantics: load existing cfg, then decode the request into
	// it. Go's json.Unmarshal leaves struct fields and map entries that
	// aren't present in the JSON untouched, so /settings POSTing just
	// `{"sandbox":{...}}` no longer wipes agents.defaults / skills.* /
	// every other namespace via saveUserConfig's namespace sweep.
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	merged, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Avoid merging stale per-agent skill entries back into the saved
	// state — those are persisted via the per-agent loop below, not
	// through the namespace sweep, and re-writing them here would
	// re-build the legacy shape we just split apart.
	merged.Skills.AgentEntries = nil
	if err := json.Unmarshal(buf, merged); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := s.saveUserConfig(r, merged); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Per-agent skill env overrides (one row per agent at scope=agent,
	// name=skills.entries). Pull from the raw body — not from the
	// merged Config — so we only touch agents the caller actually
	// patched, and don't echo every existing override back as a write.
	var raw struct {
		Skills *struct {
			AgentEntries map[string]map[string]config.SkillEntryCfg `json:"agentEntries"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(buf, &raw)
	if raw.Skills != nil && raw.Skills.AgentEntries != nil {
		for agentID, entries := range raw.Skills.AgentEntries {
			rec, err := s.dataStore.GetAgent(r.Context(), agentID)
			if err != nil || rec == nil {
				jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found: " + agentID})
				return
			}
			if !s.authorizeScope(w, r, scope.Agent, agentID) {
				return
			}
			if err := saveAgentSkillEntries(r.Context(), s.dataStore, agentID, entries); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
	}
	// Cached UserSpaces hold a snapshot of the merged config (including
	// agents.defaults.model and the provider chain). Without this, an
	// agent loaded before the change keeps seeing the stale model and
	// surfaces "no usable LLM provider" in chat.
	sc, scopeID := s.scopeForSave(r)
	s.invalidateScope(sc, scopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// scopeForSave mirrors the scope-resolution logic in saveUserConfig so
// callers can invalidate exactly the UserSpaces that were just touched.
func (s *Server) scopeForSave(r *http.Request) (string, string) {
	ident, ok := authIdentity(r)
	if ok && ident.Role == "super_admin" {
		if !ident.IsActingAs() {
			return scope.System, ""
		}
		return scope.User, ident.EffectiveUserID()
	}
	if ok {
		return scope.User, ident.UserID
	}
	return scope.User, ""
}

// --- /api/test-provider ---

type testProviderRequest struct {
	APIBase  string `json:"apiBase"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	APIType  string `json:"apiType"`
	AuthType string `json:"authType"`
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req testProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	jsonResponse(w, http.StatusOK, runProviderTest(r.Context(), req))
}

// handleTestStoredProvider runs the same connection check, but reads the
// apiKey + apiBase + apiType + authType from a saved provider row instead
// of taking them from the request body. Lets the Edit dialog test against
// the stored secret so users don't have to re-paste the key on every edit.
func (s *Server) handleTestStoredProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.Scope, rec.ScopeID) {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &pc)
	}
	jsonResponse(w, http.StatusOK, runProviderTest(r.Context(), testProviderRequest{
		APIBase:  pc.APIBase,
		APIKey:   pc.APIKey,
		Model:    body.Model,
		APIType:  pc.APIType,
		AuthType: pc.AuthType,
	}))
}

// runProviderTest issues a lightweight chat completion against the
// upstream provider. Shared between the inline test (key supplied in
// the request body, used during create / re-key) and the stored test
// (key looked up server-side, used during edit).
func runProviderTest(ctx context.Context, req testProviderRequest) map[string]any {
	base := strings.TrimRight(req.APIBase, "/")
	var testURL string
	var body io.Reader
	if req.APIType == "anthropic-messages" {
		testURL = base + "/v1/messages"
		model := req.Model
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	} else {
		testURL = base + "/chat/completions"
		model := req.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", testURL, body)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIType == "anthropic-messages" {
		httpReq.Header.Set("x-api-key", req.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else if req.AuthType == "api-key" {
		httpReq.Header.Set("api-key", req.APIKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return map[string]any{"ok": true}
	}
	respBody, _ := io.ReadAll(resp.Body)
	return map[string]any{
		"ok":    false,
		"error": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
	}
}

// --- /api/tasks ---

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskQueue == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	tasks := s.taskQueue.RecentTasks(50)
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		entry := map[string]any{
			"id":        t.ID,
			"agentId":   t.AgentID,
			"chatKey":   t.ChatKey,
			"status":    string(t.Status),
			"createdAt": t.CreatedAt.Format(time.RFC3339),
		}
		if t.StartedAt != nil && t.DoneAt != nil {
			entry["duration"] = t.DoneAt.Sub(*t.StartedAt).Milliseconds()
		}
		if t.Error != nil {
			entry["error"] = t.Error.Error()
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, out)
}

// --- chat handlers (delegate to per-user agent) ---

type chatRequest struct {
	AgentID   string   `json:"agentId,omitempty"`
	SessionID string   `json:"sessionId"`
	Message   string   `json:"message"`
	Images    []string `json:"images,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	reply := ag.HandleWebChat(r.Context(), req.SessionID, req.Message)
	jsonResponse(w, http.StatusOK, map[string]any{"reply": reply})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	events := make(chan agentChatEvent, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}()
	_ = ag.HandleWebChatStream(r.Context(), req.SessionID, req.Message, req.Images, events)
	close(events)
	<-done
}

// handleChatSubscribe holds an SSE connection open for one (agent,
// session) pair and forwards every bus.OutboundMessage routed through
// the WebChannel to that pair. Used by the dashboard chat panel to see
// cron-fired (and other async) agent replies live.
//
// Auth gating reuses resolveAgent, so the caller must already have
// permission to chat with this agent. The subscription doesn't
// generate any traffic on its own — closes are silent (client gone)
// and the server only writes when an outbound message arrives.
func (s *Server) handleChatSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.webChan == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "web channel not configured"})
		return
	}
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	if agentID == "" || sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId and sessionId required"})
		return
	}
	if ag := s.resolveAgent(r, agentID); ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Initial flush so the client EventSource fires `open` immediately
	// — without it browsers wait for the first event and the chat
	// panel can't tell whether the subscription is live yet.
	fmt.Fprintf(w, ": ok\n\n")
	flusher.Flush()

	out, unsubscribe := s.webChan.Subscribe(agentID, sessionID)
	defer unsubscribe()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-out:
			if !ok {
				return
			}
			payload := map[string]any{
				"text":      msg.Text,
				"parseMode": msg.ParseMode,
			}
			if len(msg.MediaItems) > 0 {
				items := make([]map[string]any, 0, len(msg.MediaItems))
				for _, m := range msg.MediaItems {
					items = append(items, map[string]any{
						"filename":    m.Filename,
						"contentType": m.ContentType,
					})
				}
				payload["mediaItems"] = items
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"history": ag.WebChatHistory(sessionID)})
}

func (s *Server) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"sessions": []session.WebSession{}})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"sessions": ag.WebChatSessions()})
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	var req struct{ Title string `json:"title"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := ag.RenameWebChatSession(r.PathValue("key"), req.Title); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.DeleteWebChatSession(r.PathValue("key")); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Helpers ---

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

func looksLikeSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func isMaskedSecret(s string) bool {
	if s == "" {
		return false
	}
	return s == "****" || strings.Contains(s, "****")
}

func maskSkillEntry(v config.SkillEntryCfg) config.SkillEntryCfg {
	out := config.SkillEntryCfg{Enabled: v.Enabled, APIKey: maskAPIKey(v.APIKey)}
	if len(v.Env) > 0 {
		out.Env = make(map[string]string, len(v.Env))
		for ek, ev := range v.Env {
			if looksLikeSecret(ek) {
				out.Env[ek] = maskAPIKey(ev)
			} else {
				out.Env[ek] = ev
			}
		}
	}
	return out
}

func mergeSkillEntry(existing, in config.SkillEntryCfg) config.SkillEntryCfg {
	out := config.SkillEntryCfg{Enabled: in.Enabled, APIKey: in.APIKey, Env: in.Env}
	if isMaskedSecret(out.APIKey) {
		out.APIKey = existing.APIKey
	}
	if out.Env != nil {
		for k, v := range out.Env {
			if isMaskedSecret(v) {
				out.Env[k] = existing.Env[k]
			}
		}
	}
	return out
}

func newRandID() (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func generateRandomToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "fastclaw-default-token"
	}
	return hex.EncodeToString(b)
}

// debugLog is used from various handlers for diagnostic events; kept as a
// thin wrapper so handler files don't import slog directly.
func debugLog(msg string, kv ...any) { slog.Debug(msg, kv...) }
