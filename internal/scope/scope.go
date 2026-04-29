// Package scope reads scope-tagged rows out of store.configs and
// merges them into the flat shapes the runtime expects.
//
// Every row in configs carries a (scope, scope_id, kind, name).
// Resolution walks scopes outer→inner, with inner rows shadowing outer
// rows by the row's `name`:
//
//	system → user → agent (→ skill, only for setting kinds)
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

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

const (
	System = store.ScopeSystem
	User   = store.ScopeUser
	Agent  = store.ScopeAgent
	Skill  = store.ScopeSkill
)

// Providers returns the merged map of LLM provider configs for a given
// (user, agent). Pass agentID="" to get the user-level view. Pass both
// empty to get system-only.
func Providers(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.Providers: store is required")
	}
	out := map[string]config.ProviderConfig{}
	apply := func(rows []store.ConfigRecord) {
		for _, r := range rows {
			out[r.Name] = providerToConfig(r)
		}
	}
	if rows, err := st.ListConfigs(ctx, store.KindProvider, System, ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, User, userID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, Agent, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
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
	if rows, err := st.ListConfigs(ctx, store.KindChannel, System, ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, User, userID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, Agent, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	return out, nil
}

// Setting returns the merged JSON for one namespace across system → user →
// agent → skill. Field-level merge on the top-level map; inner-scope
// fields override outer-scope ones. Unset namespaces yield an empty map
// without erroring — callers Unmarshal into typed structs and rely on
// zero-valued fields.
func Setting(ctx context.Context, st store.Store, namespace, userID, agentID, skillName string) (map[string]interface{}, error) {
	if st == nil {
		return nil, errors.New("scope.Setting: store is required")
	}
	out := map[string]interface{}{}
	merge := func(layer map[string]interface{}) {
		for k, v := range layer {
			out[k] = v
		}
	}
	if rec, err := st.GetConfigByName(ctx, store.KindSetting, System, "", namespace); err == nil && rec != nil {
		merge(rec.Data)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if userID != "" {
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, User, userID, namespace); err == nil && rec != nil {
			merge(rec.Data)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if agentID != "" {
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, Agent, agentID, namespace); err == nil && rec != nil {
			merge(rec.Data)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if skillName != "" {
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, Skill, skillName, namespace); err == nil && rec != nil {
			merge(rec.Data)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return out, nil
}

// SettingInto resolves Setting and unmarshals the merged JSON into dst.
// Convenience for callers that want a typed config block.
func SettingInto(ctx context.Context, st store.Store, namespace, userID, agentID, skillName string, dst interface{}) error {
	merged, err := Setting(ctx, st, namespace, userID, agentID, skillName)
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

// SaveSetting upserts a single namespace at the given scope. Pass nil/
// empty data to delete the row instead of writing {}.
func SaveSetting(ctx context.Context, st store.Store, scope, scopeID, namespace string, data map[string]interface{}) error {
	if st == nil {
		return errors.New("scope.SaveSetting: store is required")
	}
	if len(data) == 0 {
		// Find and drop the row if it exists. Idempotent: missing-row is a no-op.
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, scope, scopeID, namespace); err == nil && rec != nil {
			return st.DeleteConfig(ctx, rec.ID)
		}
		return nil
	}
	rec := &store.ConfigRecord{
		Kind:    store.KindSetting,
		Scope:   scope,
		ScopeID: scopeID,
		Name:    namespace,
		Enabled: true,
		Data:    data,
	}
	return st.SaveConfig(ctx, rec)
}

// SaveProvider upserts a kind="provider" row at the given scope.
func SaveProvider(ctx context.Context, st store.Store, scope, scopeID, name string, p config.ProviderConfig) error {
	rec := &store.ConfigRecord{
		Kind:    store.KindProvider,
		Scope:   scope,
		ScopeID: scopeID,
		Name:    name,
		Enabled: true,
		Data:    providerToData(p),
	}
	return st.SaveConfig(ctx, rec)
}

// SaveChannel upserts a kind="channel" row at the given scope. credentialKey
// is the stable lookup handle for inbound dispatch (bot token tail, app id).
func SaveChannel(ctx context.Context, st store.Store, scope, scopeID, channelType, credentialKey string, enabled bool, c config.ChannelConfig) error {
	rec := &store.ConfigRecord{
		Kind:          store.KindChannel,
		Scope:         scope,
		ScopeID:       scopeID,
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
