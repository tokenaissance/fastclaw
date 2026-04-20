package config

// MigrateLegacyWebSearch copies any legacy WebSearchCfg.APIKey into the new
// ToolProviders / Tools structures so older fastclaw.json files keep working
// without manual editing. Safe to call multiple times: it only runs when the
// legacy key is set AND the new structures don't already declare a provider
// for web_search.
//
// The migration is one-way. We intentionally don't rewrite the legacy field
// to empty — that would force a config save on every load.
func MigrateLegacyWebSearch(c *Config) {
	if c == nil || c.WebSearch.APIKey == "" {
		return
	}
	provider := c.WebSearch.Provider
	if provider == "" {
		provider = "brave"
	}
	if c.ToolProviders == nil {
		c.ToolProviders = map[string]ToolProviderCfg{}
	}
	if _, exists := c.ToolProviders[provider]; !exists {
		c.ToolProviders[provider] = ToolProviderCfg{APIKey: c.WebSearch.APIKey}
	}
	if c.Tools == nil {
		c.Tools = map[string]ToolCategoryCfg{}
	}
	if existing, ok := c.Tools["web_search"]; !ok || existing.Primary == "" {
		c.Tools["web_search"] = ToolCategoryCfg{Primary: provider + "/web"}
	}
}

// ResolveToolProviderCfg returns the config for a provider name, falling back
// to an empty value (not nil) so callers don't need to nil-check.
func (c *Config) ResolveToolProviderCfg(name string) ToolProviderCfg {
	if c == nil || c.ToolProviders == nil {
		return ToolProviderCfg{}
	}
	return c.ToolProviders[name]
}

// ResolveToolCategory returns the category config for categoryName (e.g.
// "web_search"), falling back to an empty value.
func (c *Config) ResolveToolCategory(categoryName string) ToolCategoryCfg {
	if c == nil || c.Tools == nil {
		return ToolCategoryCfg{}
	}
	return c.Tools[categoryName]
}
