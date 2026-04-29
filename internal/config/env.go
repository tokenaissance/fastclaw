package config

import (
	"os"
	"strconv"
)

// EnvConfig is the bootstrap configuration: storage DSN, gateway port,
// sandbox backend. Read at process start from FASTCLAW_* environment
// variables — there is no config file. Everything user-facing
// (providers, channels, agents) lives in the database.
//
// Set these as `FASTCLAW_<UPPER_SNAKE_CASE>` (or the explicit name in
// the `env:` tag below) at the process / container level. systemd unit,
// docker-compose, k8s deployment env are the canonical places.
type EnvConfig struct {
	Gateway EnvGateway
	Storage EnvStorage
	Sandbox EnvSandbox
	Log     EnvLog
}

type EnvGateway struct {
	Port int    // FASTCLAW_PORT       — default 18953
	Bind string // FASTCLAW_BIND       — "loopback" (default) or "all"
}

type EnvStorage struct {
	Type        string // FASTCLAW_STORAGE_TYPE  — "sqlite" (default) or "postgres"
	DSN         string // FASTCLAW_STORAGE_DSN   — empty = sqlite at $FASTCLAW_HOME/fastclaw.db
	AutoMigrate bool   // FASTCLAW_STORAGE_AUTO_MIGRATE — default true
}

type EnvSandbox struct {
	Enabled bool   // FASTCLAW_SANDBOX_ENABLED
	Backend string // FASTCLAW_SANDBOX_BACKEND  — "docker" or "e2b"
	Image   string // FASTCLAW_SANDBOX_IMAGE
	E2BKey  string // E2B_API_KEY
}

type EnvLog struct {
	Level string // FASTCLAW_LOG_LEVEL — "debug" / "info" / "warn" / "error"
}

// LoadEnv reads the bootstrap configuration from FASTCLAW_* environment
// variables. There is no config file: deployment-time settings are part
// of the deployment manifest (systemd / docker-compose / k8s env).
func LoadEnv() *EnvConfig {
	cfg := &EnvConfig{
		// Defaults — used when the env var isn't set. AutoMigrate=true
		// makes a fresh sqlite install boot without manual schema steps.
		Storage: EnvStorage{AutoMigrate: true},
	}

	if v := os.Getenv("FASTCLAW_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Gateway.Port = p
		}
	}
	if v := os.Getenv("FASTCLAW_BIND"); v != "" {
		cfg.Gateway.Bind = v
	}

	if v := os.Getenv("FASTCLAW_STORAGE_TYPE"); v != "" {
		cfg.Storage.Type = v
	}
	if v := os.Getenv("FASTCLAW_STORAGE_DSN"); v != "" {
		cfg.Storage.DSN = v
	}
	if v := os.Getenv("FASTCLAW_STORAGE_AUTO_MIGRATE"); v != "" {
		cfg.Storage.AutoMigrate = v == "true" || v == "1"
	}

	if v := os.Getenv("FASTCLAW_SANDBOX_ENABLED"); v != "" {
		cfg.Sandbox.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("FASTCLAW_SANDBOX_BACKEND"); v != "" {
		cfg.Sandbox.Backend = v
		// Setting a backend implies the operator wants sandbox on; this
		// mirrors the previous LoadEnv behavior.
		cfg.Sandbox.Enabled = true
	}
	if v := os.Getenv("FASTCLAW_SANDBOX_IMAGE"); v != "" {
		cfg.Sandbox.Image = v
	}
	if v := os.Getenv("E2B_API_KEY"); v != "" {
		cfg.Sandbox.E2BKey = v
	}

	if v := os.Getenv("FASTCLAW_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	return cfg
}

// applyObjectStoreEnv reads FASTCLAW_OBJECT_STORE_* env vars into cfg.
func applyObjectStoreEnv(cfg *Config) {
	read := func(key string) string { return os.Getenv("FASTCLAW_OBJECT_STORE_" + key) }
	if v := read("TYPE"); v != "" {
		cfg.ObjectStore.Type = v
	}
	if v := read("LOCAL_ROOT"); v != "" {
		cfg.ObjectStore.Local.Root = v
	}
	if v := read("REGION"); v != "" {
		cfg.ObjectStore.S3.Region = v
	}
	if v := read("BUCKET"); v != "" {
		cfg.ObjectStore.S3.Bucket = v
	}
	if v := read("PREFIX"); v != "" {
		cfg.ObjectStore.S3.Prefix = v
	}
	if v := read("ACCESSKEY"); v != "" {
		cfg.ObjectStore.S3.AccessKey = v
	}
	if v := read("SECRETKEY"); v != "" {
		cfg.ObjectStore.S3.SecretKey = v
	}
	if v := read("ACCOUNTID"); v != "" {
		cfg.ObjectStore.AccountID = v
	}
	if v := read("ENDPOINT"); v != "" {
		cfg.ObjectStore.S3.Endpoint = v
	}
	if v := read("USESSL"); v != "" {
		cfg.ObjectStore.S3.UseSSL = v == "true" || v == "1"
	}
	if v := read("ALIYUN_INTERNAL"); v != "" {
		cfg.ObjectStore.AliyunIntern = v == "true" || v == "1"
	}
}

// ApplyToConfig overlays env-derived values onto a runtime Config. Used
// by gateway boot to layer FASTCLAW_OBJECT_STORE_* on top of the DB-
// stored object-store namespace.
func (e *EnvConfig) ApplyToConfig(cfg *Config) {
	if e.Gateway.Port > 0 {
		cfg.Gateway.Port = e.Gateway.Port
	}
	if e.Gateway.Bind != "" {
		cfg.Gateway.Bind = e.Gateway.Bind
	}
	if e.Storage.Type != "" {
		cfg.Storage.Type = e.Storage.Type
	}
	if e.Storage.DSN != "" {
		cfg.Storage.DSN = e.Storage.DSN
	}
	if e.Storage.AutoMigrate {
		cfg.Storage.AutoMigrate = true
	}
	if e.Sandbox.Enabled {
		cfg.Sandbox.Enabled = true
		if e.Sandbox.Backend != "" {
			cfg.Sandbox.Backend = e.Sandbox.Backend
		}
		if e.Sandbox.Image != "" {
			cfg.Sandbox.Image = e.Sandbox.Image
		}
		if e.Sandbox.E2BKey != "" {
			cfg.Sandbox.E2BKey = e.Sandbox.E2BKey
		}
	}
	applyObjectStoreEnv(cfg)
}
