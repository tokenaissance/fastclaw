// Package scope reads (user, agent)-keyed rows out of store.configs and
// merges them into the flat shapes the runtime expects.
//
// Every row in configs carries a (kind, user_id, agent_id, name) tuple.
// Resolution walks ownership outer→inner, with inner rows shadowing outer
// rows by `name`:
//
//	system (user='', agent='') →
//	  user (user=X, agent='')   →
//	    agent (user='', agent=Y) →
//	      per-(user, agent) (user=X, agent=Y)
//
// kind="provider": name is the provider key ("openai"). Inner rows
//   replace outer entries entirely (no field-level merge).
// kind="channel":  name is the channel type ("telegram"). A disabled inner
//   row erases the outer entry — lets a user opt out of a system-wide bot.
// kind="setting":  name is the namespace ("agents.defaults", "sandbox", …).
//   Top-level keys merge field-wise; inner-scope keys win.
package scope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// HTTP-layer scope identifiers. The storage layer keys configs by
// (user_id, agent_id) directly; these constants exist for the HTTP API
// contract (URL ?scope= params, dashboard scope picker). Translate to
// storage form via OwnershipFromScope.
const (
	System    = "system"
	User      = "user"
	Agent     = "agent"
	UserAgent = "user-agent"
)

// OwnershipFromScope converts the HTTP-side (scope, scopeID) pair into
// the storage (user_id, agent_id) pair. Empty/unknown scope returns
// ("", "") which the store reads as "system / global".
func OwnershipFromScope(sc, scopeID string) (userID, agentID string) {
	switch sc {
	case User:
		return scopeID, ""
	case Agent:
		return "", scopeID
	default:
		return "", ""
	}
}

// ScopeFromOwnership is the inverse, used when emitting (scope, scopeID)
// back to the dashboard JSON. (X, Y) — both filled — is rendered as
// scope="user-agent" so the UI can tell apart per-(user, agent)
// overrides from plain user or agent rows. Today the dashboard only
// reads scope="system"/"user"/"agent"; the new compound keeps the door
// open for the multi-tenant view.
func ScopeFromOwnership(userID, agentID string) (scope, scopeID string) {
	switch {
	case userID != "" && agentID != "":
		return "user-agent", userID + "/" + agentID
	case userID != "":
		return User, userID
	case agentID != "":
		return Agent, agentID
	default:
		return System, ""
	}
}

// Providers returns the merged map of LLM provider configs for a given
// (user, agent). Pass agentID="" to get only the user-level view. Pass
// both empty to get system-only.
func Providers(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.Providers: store is required")
	}
	// Try configs_kv first — read all provider values with scope merge.
	if kvProvs, err := providersFromKV(ctx, st, userID, agentID); err == nil && len(kvProvs) > 0 {
		return kvProvs, nil
	}
	// Fallback: read from old configs table.
	out := map[string]config.ProviderConfig{}
	apply := func(rows []store.ConfigRecord) {
		for _, r := range rows {
			out[r.Name] = providerToConfig(r)
		}
	}
	// system layer
	if rows, err := st.ListConfigs(ctx, store.KindProvider, "", ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	// user layer
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, userID, ""); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	// agent layer
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, "", agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	// per-(user, agent) layer
	if userID != "" && agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, userID, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	return out, nil
}

// providersFromKV reads all provider KV values with scope merge and
// reconstructs them into ProviderConfig structs.
func providersFromKV(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ProviderConfig, error) {
	kvVals, err := GetValues(ctx, st, store.KindProvider, "", userID, agentID)
	if err != nil || len(kvVals) == 0 {
		return nil, err
	}
	// Group by provider name (first dot-segment).
	providerKVs := map[string]map[string]string{}
	for fullKey, value := range kvVals {
		idx := -1
		for i, c := range fullKey {
			if c == '.' {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		provName := fullKey[:idx]
		fieldKey := fullKey[idx+1:]
		if providerKVs[provName] == nil {
			providerKVs[provName] = map[string]string{}
		}
		providerKVs[provName][fieldKey] = value
	}
	out := make(map[string]config.ProviderConfig, len(providerKVs))
	for provName, fields := range providerKVs {
		// Reconstruct the camelCase JSON map, then unmarshal into ProviderConfig.
		m := map[string]interface{}{}
		for snakeKey, value := range fields {
			camelKey := snakeToCamel(snakeKey)
			m[camelKey] = parseKVValue(value)
		}
		blob, _ := json.Marshal(m)
		var pc config.ProviderConfig
		_ = json.Unmarshal(blob, &pc)
		out[provName] = pc
	}
	return out, nil
}

// kvValsToProviders is a helper that converts flat KV pairs (from a single
// scope) into a provider map. Used by AgentScopeProviders and
// UserScopeProviders where scope merge is not needed.
func kvValsToProviders(kvVals map[string]string) map[string]config.ProviderConfig {
	providerKVs := map[string]map[string]string{}
	for fullKey, value := range kvVals {
		idx := -1
		for i, c := range fullKey {
			if c == '.' {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		provName := fullKey[:idx]
		fieldKey := fullKey[idx+1:]
		if providerKVs[provName] == nil {
			providerKVs[provName] = map[string]string{}
		}
		providerKVs[provName][fieldKey] = value
	}
	out := make(map[string]config.ProviderConfig, len(providerKVs))
	for provName, fields := range providerKVs {
		m := map[string]interface{}{}
		for snakeKey, value := range fields {
			camelKey := snakeToCamel(snakeKey)
			m[camelKey] = parseKVValue(value)
		}
		blob, _ := json.Marshal(m)
		var pc config.ProviderConfig
		_ = json.Unmarshal(blob, &pc)
		out[provName] = pc
	}
	return out
}

// AgentScopeProviders returns providers stored at (user='', agent=Y)
// only — the agent's "official" rows, without system or user layers
// merged in. Use this to overlay an agent's own rows on top of an
// already system+user-merged view: re-running the full Providers walk
// would re-apply outer layers and silently clobber any user-scope
// override the caller already merged in.
func AgentScopeProviders(ctx context.Context, st store.Store, agentID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.AgentScopeProviders: store is required")
	}
	if agentID == "" {
		return map[string]config.ProviderConfig{}, nil
	}
	// Try configs_kv first — agent scope only.
	if kvVals, err := st.ListConfigValues(ctx, store.KindProvider, Agent, agentID, ""); err == nil && len(kvVals) > 0 {
		return kvValsToProviders(kvVals), nil
	}
	// Fallback to old configs table.
	rows, err := st.ListConfigs(ctx, store.KindProvider, "", agentID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.ProviderConfig, len(rows))
	for _, r := range rows {
		out[r.Name] = providerToConfig(r)
	}
	return out, nil
}

// UserScopeProviders returns providers stored at (user=X, agent='')
// only — the user's personal rows, without the system layer. Used by
// the foreign-agent path so a viewer can fall back to the owner's
// provider credentials without dragging the owner's full merged view
// (which would re-apply system rows on top of the viewer's already-
// merged set).
func UserScopeProviders(ctx context.Context, st store.Store, userID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.UserScopeProviders: store is required")
	}
	if userID == "" {
		return map[string]config.ProviderConfig{}, nil
	}
	// Try configs_kv first — user scope only.
	if kvVals, err := st.ListConfigValues(ctx, store.KindProvider, User, userID, ""); err == nil && len(kvVals) > 0 {
		return kvValsToProviders(kvVals), nil
	}
	// Fallback to old configs table.
	rows, err := st.ListConfigs(ctx, store.KindProvider, userID, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.ProviderConfig, len(rows))
	for _, r := range rows {
		out[r.Name] = providerToConfig(r)
	}
	return out, nil
}

// Channels returns the merged channel map. Disabled rows in an inner
// scope erase the outer entry.
func Channels(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ChannelConfig, error) {
	if st == nil {
		return nil, errors.New("scope.Channels: store is required")
	}
	out := map[string]config.ChannelConfig{}
	apply := func(rows []store.ConfigRecord) {
		for _, r := range rows {
			if !r.Enabled {
				delete(out, r.Name)
				continue
			}
			out[r.Name] = channelToConfig(r)
		}
	}
	if rows, err := st.ListConfigs(ctx, store.KindChannel, "", ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, userID, ""); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, "", agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if userID != "" && agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, userID, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	return out, nil
}

// Setting returns the merged JSON for one namespace across the
// system → user → agent → per-(user, agent) chain. Field-level merge on
// the top-level map; inner-ownership fields override outer ones. Unset
// namespaces yield an empty map without erroring — callers Unmarshal
// into typed structs and rely on zero-valued fields.
func Setting(ctx context.Context, st store.Store, namespace, userID, agentID string) (map[string]interface{}, error) {
	if st == nil {
		return nil, errors.New("scope.Setting: store is required")
	}
	// Try configs_kv first — read all values matching the namespace prefix
	// with scope merge, then reconstruct the map.
	kvPrefix := namespace + "."
	if namespace == "agents.defaults" {
		kvPrefix = "agent."
	}
	kvVals, kvErr := GetValues(ctx, st, store.KindSetting, kvPrefix, userID, agentID)
	if kvErr == nil && len(kvVals) > 0 {
		return kvToSettingMap(kvPrefix, kvVals), nil
	}
	// Fallback: read from old configs table.
	out := map[string]interface{}{}
	merge := func(layer map[string]interface{}) {
		for k, v := range layer {
			out[k] = v
		}
	}
	tryGet := func(uid, aid string) error {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, uid, aid, namespace)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		if rec != nil {
			merge(rec.Data)
		}
		return nil
	}
	if err := tryGet("", ""); err != nil {
		return nil, err
	}
	if userID != "" {
		if err := tryGet(userID, ""); err != nil {
			return nil, err
		}
	}
	if agentID != "" {
		if err := tryGet("", agentID); err != nil {
			return nil, err
		}
	}
	if userID != "" && agentID != "" {
		if err := tryGet(userID, agentID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// kvToSettingMap converts flat KV pairs back into the camelCase map that
// callers (SettingInto, assembleConfig) expect. The prefix is stripped and
// snake_case keys are converted back to camelCase. Nested dots produce
// nested maps.
func kvToSettingMap(prefix string, kv map[string]string) map[string]interface{} {
	out := map[string]interface{}{}
	for fullKey, value := range kv {
		// Strip the prefix to get the relative key.
		relKey := fullKey
		if len(prefix) > 0 && len(fullKey) > len(prefix) {
			relKey = fullKey[len(prefix):]
		}
		// Convert snake_case back to camelCase.
		camelKey := snakeToCamel(relKey)
		// Try to parse booleans and numbers back into native types.
		out[camelKey] = parseKVValue(value)
	}
	return out
}

// snakeToCamel converts a snake_case string to camelCase.
func snakeToCamel(s string) string {
	var b []byte
	upper := false
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			upper = true
			continue
		}
		if upper && s[i] >= 'a' && s[i] <= 'z' {
			b = append(b, s[i]-32)
			upper = false
		} else {
			b = append(b, s[i])
			upper = false
		}
	}
	return string(b)
}

// parseKVValue attempts to interpret a string value as its original Go type.
func parseKVValue(s string) interface{} {
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	// Try JSON array/object.
	if len(s) > 0 && (s[0] == '[' || s[0] == '{') {
		var v interface{}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v
		}
	}
	// Try number — only if the entire string is numeric.
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
		// Verify the scan consumed the whole string.
		check := fmt.Sprintf("%g", f)
		if check == s {
			return f
		}
	}
	return s
}

// SettingInto resolves Setting and unmarshals the merged JSON into dst.
// Convenience for callers that want a typed config block.
func SettingInto(ctx context.Context, st store.Store, namespace, userID, agentID string, dst interface{}) error {
	merged, err := Setting(ctx, st, namespace, userID, agentID)
	if err != nil {
		return err
	}
	if len(merged) == 0 {
		return nil
	}
	blob, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, dst)
}

// BatchSettings resolves multiple namespaces in 2 ListConfigs calls instead
// of N×2 GetConfigByName calls. It merges system-level and user-level rows
// field-wise (user wins), matching the semantics of calling Setting() for
// each namespace individually.
//
// agentID is passed through to ListConfigs — for loadUserConfig it's always "".
func BatchSettings(
	ctx context.Context,
	st store.Store,
	namespaces []string,
	userID, agentID string,
) (map[string]map[string]interface{}, error) {
	if st == nil {
		return nil, errors.New("scope.BatchSettings: store is required")
	}

	// 1 query: system-level rows (user_id="", agent_id=agentID)
	systemRows, err := st.ListConfigs(ctx, store.KindSetting, "", agentID)
	if err != nil {
		return nil, fmt.Errorf("scope.BatchSettings: load system configs: %w", err)
	}

	// 1 query: user-level rows (user_id=X, agent_id=agentID)
	var userRows []store.ConfigRecord
	if userID != "" {
		userRows, err = st.ListConfigs(ctx, store.KindSetting, userID, agentID)
		if err != nil {
			return nil, fmt.Errorf("scope.BatchSettings: load user configs: %w", err)
		}
	}

	return mergeByNamespace(systemRows, userRows, namespaces), nil
}

// mergeByNamespace groups config rows by name and merges field-wise.
// User-level values override system-level values (same semantics as Setting).
func mergeByNamespace(
	systemRows, userRows []store.ConfigRecord,
	namespaces []string,
) map[string]map[string]interface{} {
	nsSet := make(map[string]struct{}, len(namespaces))
	for _, ns := range namespaces {
		nsSet[ns] = struct{}{}
	}

	result := make(map[string]map[string]interface{}, len(namespaces))

	// Apply system layer first
	for _, row := range systemRows {
		if !row.Enabled {
			continue
		}
		if _, ok := nsSet[row.Name]; !ok {
			continue
		}
		if result[row.Name] == nil {
			result[row.Name] = make(map[string]interface{})
		}
		for k, v := range row.Data {
			result[row.Name][k] = v
		}
	}

	// Apply user layer (overrides system)
	for _, row := range userRows {
		if !row.Enabled {
			continue
		}
		if _, ok := nsSet[row.Name]; !ok {
			continue
		}
		if result[row.Name] == nil {
			result[row.Name] = make(map[string]interface{})
		}
		for k, v := range row.Data {
			result[row.Name][k] = v
		}
	}

	return result
}

// SaveSettingByScope is the legacy (scope, scopeID) form kept for the
// HTTP layer, which still emits scope strings in URL params and JSON.
// New callers should use SaveSetting with explicit (userID, agentID).
func SaveSettingByScope(ctx context.Context, st store.Store, sc, scopeID, namespace string, data map[string]interface{}) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveSetting(ctx, st, uid, aid, namespace, data)
}

// SaveProviderByScope / SaveChannelByScope mirror the same legacy bridge.
func SaveProviderByScope(ctx context.Context, st store.Store, sc, scopeID, name string, p config.ProviderConfig) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveProvider(ctx, st, uid, aid, name, p)
}

func SaveChannelByScope(ctx context.Context, st store.Store, sc, scopeID, channelType, credentialKey string, enabled bool, c config.ChannelConfig) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveChannel(ctx, st, uid, aid, channelType, credentialKey, enabled, c)
}

// SaveSetting upserts a single namespace at the given (user, agent)
// ownership. Pass nil/empty data to delete the row instead of writing
// {}. Pass empty userID/agentID for system-level.
func SaveSetting(ctx context.Context, st store.Store, userID, agentID, namespace string, data map[string]interface{}) error {
	if st == nil {
		return errors.New("scope.SaveSetting: store is required")
	}
	// Dual-write to configs_kv.
	dualWriteSettingKV(ctx, st, userID, agentID, namespace, data)
	if len(data) == 0 {
		// Find and drop the row if it exists. Idempotent: missing-row is a no-op.
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, userID, agentID, namespace); err == nil && rec != nil {
			return st.DeleteConfig(ctx, rec.ID)
		}
		return nil
	}
	rec := &store.ConfigRecord{
		Kind:    store.KindSetting,
		UserID:  userID,
		AgentID: agentID,
		Name:    namespace,
		Enabled: true,
		Data:    data,
	}
	return st.SaveConfig(ctx, rec)
}

// SaveProvider upserts a kind="provider" row at the given (user, agent)
// ownership.
func SaveProvider(ctx context.Context, st store.Store, userID, agentID, name string, p config.ProviderConfig) error {
	// Dual-write to configs_kv.
	dualWriteProviderKV(ctx, st, userID, agentID, name, p)
	rec := &store.ConfigRecord{
		Kind:    store.KindProvider,
		UserID:  userID,
		AgentID: agentID,
		Name:    name,
		Enabled: true,
		Data:    providerToData(p),
	}
	return st.SaveConfig(ctx, rec)
}

// SaveChannel upserts a kind="channel" row at the given (user, agent)
// ownership. credentialKey is the stable lookup handle for inbound
// dispatch (bot token tail, app id).
func SaveChannel(ctx context.Context, st store.Store, userID, agentID, channelType, credentialKey string, enabled bool, c config.ChannelConfig) error {
	rec := &store.ConfigRecord{
		Kind:          store.KindChannel,
		UserID:        userID,
		AgentID:       agentID,
		Name:          channelType,
		Enabled:       enabled,
		CredentialKey: credentialKey,
		Data:          channelToData(c),
	}
	return st.SaveConfig(ctx, rec)
}

func providerToConfig(r store.ConfigRecord) config.ProviderConfig {
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(r.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &pc)
	}
	return pc
}

func providerToData(p config.ProviderConfig) map[string]interface{} {
	blob, _ := json.Marshal(p)
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	return m
}

func channelToConfig(r store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: r.Enabled}
	if blob, err := json.Marshal(r.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = r.Enabled
	return cc
}

func channelToData(c config.ChannelConfig) map[string]interface{} {
	blob, _ := json.Marshal(c)
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	delete(m, "enabled") // enabled lives on the row column, not in data
	return m
}

// ---------------------------------------------------------------------------
// configs_kv read/write helpers
// ---------------------------------------------------------------------------

// kvScopeFromOwnership converts (userID, agentID) into the configs_kv
// (scope, scope_id) pair. Mirrors ScopeFromOwnership so the per-(user,
// agent) layer survives the configs_kv dual-write — without it, a key
// bound at per-(user, agent) scope would be flattened to the user layer
// and leak across the user's agents.
func kvScopeFromOwnership(userID, agentID string) (scope, scopeID string) {
	switch {
	case userID != "" && agentID != "":
		return UserAgent, userID + "/" + agentID
	case userID != "":
		return User, userID
	case agentID != "":
		return Agent, agentID
	default:
		return System, ""
	}
}

// GetValue reads a single config value with scope precedence:
// system → user → agent → per-(user,agent). Inner scope wins.
func GetValue(ctx context.Context, st store.Store, kind, name, userID, agentID string) (string, bool, error) {
	if st == nil {
		return "", false, errors.New("scope.GetValue: store is required")
	}
	var found bool
	var value string
	tryGet := func(sc, sid string) error {
		v, err := st.GetConfigValue(ctx, kind, sc, sid, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		value = v
		found = true
		return nil
	}
	if err := tryGet(System, ""); err != nil {
		return "", false, err
	}
	if userID != "" {
		if err := tryGet(User, userID); err != nil {
			return "", false, err
		}
	}
	if agentID != "" {
		if err := tryGet(Agent, agentID); err != nil {
			return "", false, err
		}
	}
	if userID != "" && agentID != "" {
		if err := tryGet(UserAgent, userID+"/"+agentID); err != nil {
			return "", false, err
		}
	}
	return value, found, nil
}

// GetValues reads all values matching a prefix with scope merge.
// System values are read first, then user overrides, then agent overrides.
// For each name key, the innermost scope wins.
func GetValues(ctx context.Context, st store.Store, kind, prefix, userID, agentID string) (map[string]string, error) {
	if st == nil {
		return nil, errors.New("scope.GetValues: store is required")
	}
	out := map[string]string{}
	merge := func(sc, sid string) error {
		m, err := st.ListConfigValues(ctx, kind, sc, sid, prefix)
		if err != nil {
			return err
		}
		for k, v := range m {
			out[k] = v
		}
		return nil
	}
	if err := merge(System, ""); err != nil {
		return nil, err
	}
	if userID != "" {
		if err := merge(User, userID); err != nil {
			return nil, err
		}
	}
	if agentID != "" {
		if err := merge(Agent, agentID); err != nil {
			return nil, err
		}
	}
	if userID != "" && agentID != "" {
		if err := merge(UserAgent, userID+"/"+agentID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetValue writes a single config value at the specified scope.
func SetValue(ctx context.Context, st store.Store, kind, scope, scopeID, name, value string) error {
	if st == nil {
		return errors.New("scope.SetValue: store is required")
	}
	return st.SetConfigValue(ctx, kind, scope, scopeID, name, value)
}

// camelToSnake converts a camelCase string to snake_case.
func camelToSnake(s string) string {
	var b []byte
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b = append(b, '_')
			}
			b = append(b, byte(r+32))
		} else {
			b = append(b, byte(r))
		}
	}
	return string(b)
}

// flattenJSONToKV flattens a map into dotted-key → string pairs, used for
// dual-writing to configs_kv. Same logic as store.flattenJSON.
func flattenJSONToKV(prefix string, data map[string]interface{}, out map[string]string) {
	for k, v := range data {
		snakeKey := camelToSnake(k)
		fullKey := prefix + snakeKey
		switch val := v.(type) {
		case map[string]interface{}:
			flattenJSONToKV(fullKey+".", val, out)
		case string:
			out[fullKey] = val
		case bool:
			if val {
				out[fullKey] = "true"
			} else {
				out[fullKey] = "false"
			}
		case float64:
			out[fullKey] = fmt.Sprintf("%g", val)
		case nil:
			// skip
		default:
			blob, _ := json.Marshal(val)
			out[fullKey] = string(blob)
		}
	}
}

// dualWriteSettingKV writes the flattened KV pairs to configs_kv alongside
// the legacy configs table write. Called by SaveSetting to keep both tables
// in sync during migration.
func dualWriteSettingKV(ctx context.Context, st store.Store, userID, agentID, namespace string, data map[string]interface{}) {
	sc, sid := kvScopeFromOwnership(userID, agentID)
	// Determine the KV name prefix.
	kvPrefix := namespace + "."
	if namespace == "agents.defaults" {
		kvPrefix = "agent."
	}
	if len(data) == 0 {
		// Delete all KV entries for this prefix.
		_ = st.DeleteConfigPrefix(ctx, store.KindSetting, sc, sid, kvPrefix)
		return
	}
	flat := map[string]string{}
	flattenJSONToKV(kvPrefix, data, flat)
	// Delete stale keys then write new ones.
	_ = st.DeleteConfigPrefix(ctx, store.KindSetting, sc, sid, kvPrefix)
	for name, value := range flat {
		_ = st.SetConfigValue(ctx, store.KindSetting, sc, sid, name, value)
	}
}

// dualWriteProviderKV writes the flattened provider config to configs_kv.
func dualWriteProviderKV(ctx context.Context, st store.Store, userID, agentID, providerName string, p config.ProviderConfig) {
	sc, sid := kvScopeFromOwnership(userID, agentID)
	kvPrefix := providerName + "."
	data := providerToData(p)
	flat := map[string]string{}
	flattenJSONToKV(kvPrefix, data, flat)
	_ = st.DeleteConfigPrefix(ctx, store.KindProvider, sc, sid, kvPrefix)
	for name, value := range flat {
		_ = st.SetConfigValue(ctx, store.KindProvider, sc, sid, name, value)
	}
}

// DualDeleteProviderKV removes all KV entries for a provider.
func DualDeleteProviderKV(ctx context.Context, st store.Store, userID, agentID, providerName string) {
	sc, sid := kvScopeFromOwnership(userID, agentID)
	_ = st.DeleteConfigPrefix(ctx, store.KindProvider, sc, sid, providerName+".")
}
