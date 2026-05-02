package localagents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// InitOptions controls direct local agent configuration.
type InitOptions struct {
	Home        string
	Port        int
	AgentName   string
	Description string

	Provider  string
	Model     string
	APIKeyEnv string
	APIBase   string
	APIType   string
	AuthType  string

	Username    string
	Email       string
	Password    string
	DisplayName string

	SandboxEnabled bool
	SandboxBackend string
	SandboxImage   string
	SandboxNetwork string
}

// InitResult describes what init created or updated.
type InitResult struct {
	Instance          Instance
	CreatedUser       bool
	GeneratedPassword string
	ProviderSaved     bool
	ModelSaved        bool
}

// Init creates or updates the sqlite-backed configuration for a local agent.
func Init(name string, opts InitOptions) (*InitResult, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	p, err := instancePaths(name)
	if err != nil {
		return nil, err
	}
	existing, err := loadInstance(p.metaFile)
	if errors.Is(err, os.ErrNotExist) {
		existing = nil
	} else if err != nil {
		return nil, err
	}
	if existing != nil && isProcessAlive(existing.PID) {
		if opts.Home != "" && expandHome(opts.Home) != existing.Home {
			return nil, fmt.Errorf("agent %q is running; stop it before changing --home", name)
		}
		if opts.Port > 0 && opts.Port != existing.Port {
			return nil, fmt.Errorf("agent %q is running; stop it before changing --port", name)
		}
	}
	home := opts.Home
	if home == "" && existing != nil {
		home = existing.Home
	}
	if home == "" {
		home = p.homeDir
	}
	home = expandHome(home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("create agent home: %w", err)
	}

	providerSaved := false
	modelSaved := false
	var providerName string
	var modelID string
	var fullModel string
	if opts.Provider != "" || opts.Model != "" {
		providerName, modelID, fullModel, err = normalizeProviderModel(opts.Provider, opts.Model)
		if err != nil {
			return nil, err
		}
	}

	st, err := openInstanceStore(home)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	ctx := context.Background()
	acct, created, generatedPassword, err := ensureAccount(ctx, st, opts)
	if err != nil {
		return nil, err
	}

	agentID := "agt_" + name
	if existing != nil && existing.AgentID != "" {
		agentID = existing.AgentID
	}
	agentName := opts.AgentName
	if agentName == "" && existing != nil {
		agentName = existing.AgentName
	}
	if agentName == "" {
		agentName = name
	}
	agentConfig := map[string]interface{}{}
	ownerID := acct.ID
	if rec, err := st.GetAgent(ctx, agentID); err == nil && rec != nil {
		if rec.Config != nil {
			agentConfig = rec.Config
		}
		// Re-init must not silently rebind an agent record onto a
		// different user. Honour an explicit --username switch (which
		// sets opts.Username) but otherwise keep the existing owner.
		if rec.UserID != "" && rec.UserID != acct.ID {
			if opts.Username == "" {
				ownerID = rec.UserID
			} else {
				return nil, fmt.Errorf("agent %q is owned by user %s; pass --username matching that account or remove the agent first", name, rec.UserID)
			}
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	agentRec := &store.AgentRecord{
		ID:     agentID,
		UserID: ownerID,
		Name:   agentName,
		Config: agentConfig,
	}
	if opts.Description != "" {
		agentRec.Config["description"] = opts.Description
	}
	if err := st.SaveAgent(ctx, agentRec); err != nil {
		return nil, err
	}

	if opts.Provider != "" || opts.Model != "" {
		pcfg, err := providerConfigFromOptions(ctx, st, providerName, modelID, opts)
		if err != nil {
			return nil, err
		}
		if err := scope.SaveProvider(ctx, st, scope.System, "", providerName, pcfg); err != nil {
			return nil, err
		}
		providerSaved = true
		if fullModel != "" {
			if err := saveSettingValue(ctx, st, "agents.defaults", []string{"model"}, fullModel); err != nil {
				return nil, err
			}
			modelSaved = true
		}
	}
	if opts.SandboxEnabled {
		data, err := loadSettingMap(ctx, st, "sandbox")
		if err != nil {
			return nil, err
		}
		data["enabled"] = true
		data["backend"] = defaultStr(opts.SandboxBackend, "docker")
		if opts.SandboxImage != "" {
			data["image"] = opts.SandboxImage
		}
		if opts.SandboxNetwork != "" {
			data["network"] = opts.SandboxNetwork
		}
		if err := scope.SaveSetting(ctx, st, scope.System, "", "sandbox", data); err != nil {
			return nil, err
		}
	}

	port := opts.Port
	if port <= 0 && existing != nil {
		port = existing.Port
	}
	inst := &Instance{
		Name:      name,
		AgentID:   agentID,
		AgentName: agentName,
		UserID:    ownerID,
		Port:      port,
		Home:      home,
		LogFile:   p.logFile,
	}
	if port > 0 {
		inst.URL = fmt.Sprintf("http://localhost:%d", port)
	}
	if existing != nil {
		inst.PID = existing.PID
		inst.Command = existing.Command
		inst.StoppedAt = existing.StoppedAt
		if inst.StartedAt.IsZero() {
			inst.StartedAt = existing.StartedAt
		}
	}
	if err := saveInstance(p.metaFile, inst); err != nil {
		return nil, err
	}
	return &InitResult{
		Instance:          *inst,
		CreatedUser:       created,
		GeneratedPassword: generatedPassword,
		ProviderSaved:     providerSaved,
		ModelSaved:        modelSaved,
	}, nil
}

// SetConfig writes a supported provider or setting key.
func SetConfig(name, key, rawValue string) error {
	st, _, err := storeForName(name)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	if strings.HasPrefix(key, "provider.") {
		return setProviderField(ctx, st, key, rawValue)
	}
	namespace, path, err := settingKey(key)
	if err != nil {
		return err
	}
	if len(path) == 0 {
		obj, ok := parseValue(rawValue).(map[string]interface{})
		if !ok {
			return fmt.Errorf("config key %q expects a JSON object value", key)
		}
		return scope.SaveSetting(ctx, st, scope.System, "", namespace, obj)
	}
	data := map[string]interface{}{}
	if rec, err := st.GetConfigByName(ctx, store.KindSetting, scope.System, "", namespace); err == nil && rec != nil {
		data = rec.Data
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	setNested(data, path, parseValue(rawValue))
	if namespace == "sandbox" && len(path) > 0 && path[0] == "enabled" {
		if enabled, _ := data["enabled"].(bool); enabled {
			if _, ok := data["backend"].(string); !ok {
				data["backend"] = "docker"
			}
		}
	}
	return scope.SaveSetting(ctx, st, scope.System, "", namespace, data)
}

// GetConfig returns one config value, or a redacted system config dump when key is empty.
func GetConfig(name, key string) (interface{}, error) {
	st, _, err := storeForName(name)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	ctx := context.Background()
	if key == "" {
		return configDump(ctx, st)
	}
	if strings.HasPrefix(key, "provider.") {
		return getProviderField(ctx, st, key)
	}
	namespace, path, err := settingKey(key)
	if err != nil {
		return nil, err
	}
	rec, err := st.GetConfigByName(ctx, store.KindSetting, scope.System, "", namespace)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return rec.Data, nil
	}
	return getNested(rec.Data, path), nil
}

// PutFile writes an agent system file for the configured local agent.
func PutFile(name, filename, srcPath string) error {
	if err := validateSystemFilename(filename); err != nil {
		return err
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	st, inst, err := storeForName(name)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := ensureConfigured(inst); err != nil {
		return err
	}
	return st.SaveAgentFile(context.Background(), inst.AgentID, inst.UserID, filename, data)
}

// GetFile reads an agent system file.
func GetFile(name, filename string) ([]byte, error) {
	if err := validateSystemFilename(filename); err != nil {
		return nil, err
	}
	st, inst, err := storeForName(name)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := ensureConfigured(inst); err != nil {
		return nil, err
	}
	return st.GetAgentFile(context.Background(), inst.AgentID, inst.UserID, filename)
}

// ListFiles lists agent system files stored for the configured local agent.
func ListFiles(name string) ([]string, error) {
	st, inst, err := storeForName(name)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if err := ensureConfigured(inst); err != nil {
		return nil, err
	}
	files, err := st.ListAgentFiles(context.Background(), inst.AgentID, inst.UserID)
	if err != nil {
		return nil, err
	}
	out := files[:0]
	for _, file := range files {
		if systemFileAllowlist[file] {
			out = append(out, file)
		}
	}
	return out, nil
}

func openInstanceStore(home string) (store.Store, error) {
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prevLogger)
	return store.New(&store.StorageConfig{
		Type:        store.StorageSQLite,
		AutoMigrate: true,
	}, home)
}

func storeForName(name string) (store.Store, *Instance, error) {
	if err := validateName(name); err != nil {
		return nil, nil, err
	}
	p, err := instancePaths(name)
	if err != nil {
		return nil, nil, err
	}
	inst, err := loadInstance(p.metaFile)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %q is not initialized; run `fastclaw agents init %s` first", name, name)
	}
	if inst.Home == "" {
		inst.Home = p.homeDir
	}
	st, err := openInstanceStore(inst.Home)
	if err != nil {
		return nil, nil, err
	}
	return st, inst, nil
}

func ensureAccount(ctx context.Context, st store.Store, opts InitOptions) (*users.Account, bool, string, error) {
	accts, err := users.NewAccounts(st)
	if err != nil {
		return nil, false, "", err
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		return nil, false, "", err
	}
	if opts.Username != "" {
		rec, err := st.GetUserByLogin(ctx, opts.Username)
		if err == nil {
			acct, err := accts.Get(ctx, rec.ID)
			return acct, false, "", err
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, false, "", err
		}
		// Refusing the silent fallback: when the local DB already has
		// users, --username pointing at a missing account must fail loudly
		// instead of bonding the agent to whichever super-admin we find.
		if n > 0 {
			return nil, false, "", fmt.Errorf("user %q not found in local database", opts.Username)
		}
	}
	if n > 0 {
		usersList, err := accts.List(ctx)
		if err != nil {
			return nil, false, "", err
		}
		if len(usersList) == 0 {
			return nil, false, "", errors.New("users list is empty despite nonzero count")
		}
		for _, acct := range usersList {
			if acct.Role == users.RoleSuperAdmin {
				return acct, false, "", nil
			}
		}
		return usersList[0], false, "", nil
	}

	username := defaultStr(opts.Username, "admin")
	email := defaultStr(opts.Email, "admin@local.fastclaw")
	password := opts.Password
	generated := ""
	if password == "" {
		var err error
		password, err = randomPassword()
		if err != nil {
			return nil, false, "", err
		}
		generated = password
	}
	acct, err := accts.Create(ctx, username, email, password, opts.DisplayName, users.RoleSuperAdmin)
	return acct, err == nil, generated, err
}

func normalizeProviderModel(providerName, model string) (string, string, string, error) {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)
	modelID := model
	if strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("invalid model %q: expected <provider>/<model>", model)
		}
		if providerName != "" && providerName != parts[0] {
			return "", "", "", fmt.Errorf("--provider %q does not match model provider prefix %q", providerName, parts[0])
		}
		if providerName == "" {
			providerName = parts[0]
		}
		modelID = parts[1]
	}
	if providerName == "" {
		return "", "", "", errors.New("--provider is required when --model does not include a provider prefix")
	}
	fullModel := ""
	if modelID != "" {
		fullModel = providerName + "/" + modelID
	}
	return providerName, modelID, fullModel, nil
}

func providerConfigFromOptions(ctx context.Context, st store.Store, providerName, modelID string, opts InitOptions) (config.ProviderConfig, error) {
	preset := providerPreset(providerName)
	existing := config.ProviderConfig{}
	if rec, err := st.GetConfigByName(ctx, store.KindProvider, scope.System, "", providerName); err == nil && rec != nil {
		blob, _ := json.Marshal(rec.Data)
		_ = json.Unmarshal(blob, &existing)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return config.ProviderConfig{}, err
	}
	apiBase := firstNonEmpty(opts.APIBase, existing.APIBase, preset.apiBase)
	apiType := firstNonEmpty(opts.APIType, existing.APIType, preset.apiType)
	authType := firstNonEmpty(opts.AuthType, existing.AuthType, preset.authType)
	apiKeyEnv := opts.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = preset.apiKeyEnv
	}
	apiKey := existing.APIKey
	if opts.APIKeyEnv != "" || (apiKey == "" && apiKeyEnv != "") {
		apiKey = os.Getenv(apiKeyEnv)
		if apiKey == "" && providerName != "ollama" {
			return config.ProviderConfig{}, fmt.Errorf("environment variable %s is empty", apiKeyEnv)
		}
	}
	if apiKey == "" && providerName == "ollama" {
		apiKey = "ollama"
	}
	cfg := config.ProviderConfig{
		APIKey:   apiKey,
		APIBase:  apiBase,
		APIType:  apiType,
		AuthType: authType,
		Models:   existing.Models,
	}
	if modelID != "" {
		cfg.Models = appendModel(cfg.Models, modelID)
	}
	return cfg, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type providerDefaults struct {
	apiBase   string
	apiType   string
	authType  string
	apiKeyEnv string
}

func providerPreset(name string) providerDefaults {
	switch strings.ToLower(name) {
	case "anthropic":
		return providerDefaults{"https://api.anthropic.com", "anthropic-messages", "api-key", "ANTHROPIC_API_KEY"}
	case "openrouter":
		return providerDefaults{"https://openrouter.ai/api/v1", "openai-chat", "bearer-token", "OPENROUTER_API_KEY"}
	case "ollama":
		return providerDefaults{"http://localhost:11434/v1", "openai-chat", "bearer-token", ""}
	case "groq":
		return providerDefaults{"https://api.groq.com/openai/v1", "openai-chat", "bearer-token", "GROQ_API_KEY"}
	case "deepseek":
		return providerDefaults{"https://api.deepseek.com/v1", "openai-chat", "bearer-token", "DEEPSEEK_API_KEY"}
	case "mistral":
		return providerDefaults{"https://api.mistral.ai/v1", "openai-chat", "bearer-token", "MISTRAL_API_KEY"}
	case "openai":
		fallthrough
	default:
		return providerDefaults{"https://api.openai.com/v1", "openai-chat", "bearer-token", "OPENAI_API_KEY"}
	}
}

func setProviderField(ctx context.Context, st store.Store, key, rawValue string) error {
	parts := strings.Split(strings.TrimPrefix(key, "provider."), ".")
	if len(parts) != 2 {
		return errors.New("provider config key must look like provider.<name>.<field>")
	}
	name, field := parts[0], parts[1]
	pc := config.ProviderConfig{}
	if rec, err := st.GetConfigByName(ctx, store.KindProvider, scope.System, "", name); err == nil && rec != nil {
		blob, _ := json.Marshal(rec.Data)
		_ = json.Unmarshal(blob, &pc)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	} else {
		pc = providerConfigFromPreset(name)
	}
	switch field {
	case "apiKeyEnv":
		pc.APIKey = os.Getenv(rawValue)
		if pc.APIKey == "" {
			return fmt.Errorf("environment variable %s is empty", rawValue)
		}
	case "apiKey":
		pc.APIKey = rawValue
	case "apiBase":
		pc.APIBase = rawValue
	case "apiType":
		pc.APIType = rawValue
	case "authType":
		pc.AuthType = rawValue
	case "model":
		if rawValue == "" {
			return errors.New("provider model id is required; pass `provider.<name>.models` (note the s) with an empty array to clear the list")
		}
		pc.Models = appendModel(pc.Models, rawValue)
	case "models":
		if rawValue == "[]" {
			pc.Models = nil
		} else {
			return errors.New(`only "[]" is accepted for provider.<name>.models — use provider.<name>.model <id> to add entries`)
		}
	default:
		return fmt.Errorf("unsupported provider field %q", field)
	}
	return scope.SaveProvider(ctx, st, scope.System, "", name, pc)
}

func providerConfigFromPreset(providerName string) config.ProviderConfig {
	preset := providerPreset(providerName)
	return config.ProviderConfig{
		APIBase:  preset.apiBase,
		APIType:  preset.apiType,
		AuthType: preset.authType,
	}
}

func getProviderField(ctx context.Context, st store.Store, key string) (interface{}, error) {
	parts := strings.Split(strings.TrimPrefix(key, "provider."), ".")
	if len(parts) != 2 {
		return nil, errors.New("provider config key must look like provider.<name>.<field>")
	}
	rec, err := st.GetConfigByName(ctx, store.KindProvider, scope.System, "", parts[0])
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := redactProviderData(rec.Data)
	return data[parts[1]], nil
}

func appendModel(models []config.ModelEntry, id string) []config.ModelEntry {
	for _, m := range models {
		if m.ID == id {
			return models
		}
	}
	return append(models, config.ModelEntry{ID: id, Name: id})
}

var knownSettingNamespaces = []string{
	"agents.defaults",
	"skills.install",
	"skills.entries",
	"tools.providers",
	"tools.categories",
	"skillsLearner",
	"objectstore",
	"taskqueue",
	"sandbox",
	"heartbeat",
	"plugins",
	"memory",
	"privacy",
	"hooks",
	"teams",
	"bindings",
}

func settingKey(key string) (string, []string, error) {
	switch key {
	case "model":
		return "agents.defaults", []string{"model"}, nil
	case "maxTokens":
		return "agents.defaults", []string{"maxTokens"}, nil
	case "temperature":
		return "agents.defaults", []string{"temperature"}, nil
	case "thinking":
		return "agents.defaults", []string{"thinking"}, nil
	case "policy":
		return "agents.defaults", []string{"policy"}, nil
	}
	for _, ns := range knownSettingNamespaces {
		if key == ns {
			return ns, nil, nil
		}
		prefix := ns + "."
		if strings.HasPrefix(key, prefix) {
			path := strings.Split(strings.TrimPrefix(key, prefix), ".")
			if len(path) == 0 || path[0] == "" {
				break
			}
			return ns, path, nil
		}
	}
	return "", nil, fmt.Errorf("unsupported config key %q", key)
}

func configDump(ctx context.Context, st store.Store) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	settings, err := st.ListConfigs(ctx, store.KindSetting, scope.System, "")
	if err != nil {
		return nil, err
	}
	for _, rec := range settings {
		out[rec.Name] = rec.Data
	}
	providers, err := st.ListConfigs(ctx, store.KindProvider, scope.System, "")
	if err != nil {
		return nil, err
	}
	provOut := map[string]interface{}{}
	for _, rec := range providers {
		provOut[rec.Name] = redactProviderData(rec.Data)
	}
	if len(provOut) > 0 {
		out["providers"] = provOut
	}
	agents, err := st.ListAllAgents(ctx)
	if err == nil && len(agents) > 0 {
		sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
		out["agents"] = agents
	}
	return out, nil
}

func redactProviderData(data map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		if k == "apiKey" {
			if s, _ := v.(string); s != "" {
				out[k] = "<set>"
			} else {
				out[k] = ""
			}
			continue
		}
		out[k] = v
	}
	return out
}

func setNested(data map[string]interface{}, path []string, value interface{}) {
	cur := data
	for _, p := range path[:len(path)-1] {
		next, _ := cur[p].(map[string]interface{})
		if next == nil {
			next = map[string]interface{}{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

func loadSettingMap(ctx context.Context, st store.Store, namespace string) (map[string]interface{}, error) {
	if rec, err := st.GetConfigByName(ctx, store.KindSetting, scope.System, "", namespace); err == nil && rec != nil {
		if rec.Data != nil {
			return rec.Data, nil
		}
		return map[string]interface{}{}, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return map[string]interface{}{}, nil
}

func saveSettingValue(ctx context.Context, st store.Store, namespace string, path []string, value interface{}) error {
	data, err := loadSettingMap(ctx, st, namespace)
	if err != nil {
		return err
	}
	if len(path) == 0 {
		obj, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("config namespace %q expects a JSON object value", namespace)
		}
		data = obj
	} else {
		setNested(data, path, value)
	}
	return scope.SaveSetting(ctx, st, scope.System, "", namespace, data)
}

func getNested(data map[string]interface{}, path []string) interface{} {
	var cur interface{} = data
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func parseValue(raw string) interface{} {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}
	if i, err := strconv.Atoi(raw); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return raw
}

func ensureConfigured(inst *Instance) error {
	if inst == nil || inst.AgentID == "" || inst.UserID == "" {
		return errors.New("agent is not initialized; run `fastclaw agents init <name>` first")
	}
	return nil
}

var systemFileAllowlist = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
	"agent.json":   true,
}

func validateSystemFilename(filename string) error {
	if !systemFileAllowlist[filename] {
		return fmt.Errorf("filename %q is not a supported agent system file", filename)
	}
	return nil
}

func randomPassword() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
