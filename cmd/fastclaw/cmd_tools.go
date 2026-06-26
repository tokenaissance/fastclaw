package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

type toolsConfigFile struct {
	ToolProviders map[string]config.ToolProviderCfg `json:"toolProviders"`
	Tools         map[string]config.ToolCategoryCfg `json:"tools"`
}

func toolsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tools", Short: "Manage provider-backed tool settings"}
	cmd.AddCommand(toolsGetCmd(), toolsPutCmd(), toolsProviderSetCmd(), toolsCategorySetCmd())
	return cmd
}

func toolsGetCmd() *cobra.Command {
	var userID, agentID string
	var merged bool
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Print tool provider and category settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			out := toolsConfigFile{
				ToolProviders: map[string]config.ToolProviderCfg{},
				Tools:         map[string]config.ToolCategoryCfg{},
			}
			if merged {
				if err := scope.SettingInto(ctx, st, gateway.NSToolProviders, userID, agentID, &out.ToolProviders); err != nil {
					return err
				}
				if err := scope.SettingInto(ctx, st, gateway.NSToolCategories, userID, agentID, &out.Tools); err != nil {
					return err
				}
			} else {
				out.ToolProviders, err = loadToolProvidersExact(ctx, st, userID, agentID)
				if err != nil {
					return err
				}
				out.Tools, err = loadToolCategoriesExact(ctx, st, userID, agentID)
				if err != nil {
					return err
				}
			}
			return printValue(out)
		},
	}
	addToolScopeFlags(cmd, &userID, &agentID)
	cmd.Flags().BoolVar(&merged, "merged", false, "read merged system/user/agent settings instead of exact scope row")
	return cmd
}

func toolsPutCmd() *cobra.Command {
	var userID, agentID string
	cmd := &cobra.Command{
		Use:   "put <json-path>",
		Short: "Replace tool settings from a JSON file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var in toolsConfigFile
			if err := json.Unmarshal(data, &in); err != nil {
				return err
			}
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			if err := saveToolProviders(ctx, st, userID, agentID, in.ToolProviders); err != nil {
				return err
			}
			if err := saveToolCategories(ctx, st, userID, agentID, in.Tools); err != nil {
				return err
			}
			fmt.Println("Saved tool settings")
			notifyGatewayReload()
			return nil
		},
	}
	addToolScopeFlags(cmd, &userID, &agentID)
	return cmd
}

func toolsProviderSetCmd() *cobra.Command {
	var userID, agentID, endpoint, apiKey, optionsJSON string
	cmd := &cobra.Command{
		Use:   "provider-set <name>",
		Short: "Create or update one tool provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			providers, err := loadToolProvidersExact(ctx, st, userID, agentID)
			if err != nil {
				return err
			}
			cfg := providers[args[0]]
			cfg.Endpoint = endpoint
			cfg.APIKey = apiKey
			if optionsJSON != "" {
				var opts map[string]string
				if err := json.Unmarshal([]byte(optionsJSON), &opts); err != nil {
					return err
				}
				cfg.Options = opts
			}
			providers[args[0]] = cfg
			if err := saveToolProviders(ctx, st, userID, agentID, providers); err != nil {
				return err
			}
			fmt.Printf("Saved tool provider %s\n", args[0])
			notifyGatewayReload()
			return nil
		},
	}
	addToolScopeFlags(cmd, &userID, &agentID)
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "provider endpoint/base URL")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "provider API key")
	cmd.Flags().StringVar(&optionsJSON, "options-json", "", "provider options as a JSON object")
	return cmd
}

func toolsCategorySetCmd() *cobra.Command {
	var userID, agentID, primary string
	var fallbacks []string
	var autoFallback bool
	cmd := &cobra.Command{
		Use:   "category-set <category>",
		Short: "Create or update one tool category chain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			cats, err := loadToolCategoriesExact(ctx, st, userID, agentID)
			if err != nil {
				return err
			}
			cats[args[0]] = config.ToolCategoryCfg{
				Primary:      primary,
				Fallbacks:    fallbacks,
				AutoFallback: &autoFallback,
			}
			if err := saveToolCategories(ctx, st, userID, agentID, cats); err != nil {
				return err
			}
			fmt.Printf("Saved tool category %s\n", args[0])
			notifyGatewayReload()
			return nil
		},
	}
	addToolScopeFlags(cmd, &userID, &agentID)
	cmd.Flags().StringVar(&primary, "primary", "", "primary provider name or provider/model ref")
	cmd.Flags().StringSliceVar(&fallbacks, "fallback", nil, "fallback provider ref; repeat or comma-separate")
	cmd.Flags().BoolVar(&autoFallback, "auto-fallback", true, "allow automatic fallback when a provider returns no result")
	_ = cmd.MarkFlagRequired("primary")
	return cmd
}

func addToolScopeFlags(cmd *cobra.Command, userID, agentID *string) {
	cmd.Flags().StringVar(userID, "user", "", "user id for user-scoped settings")
	cmd.Flags().StringVar(agentID, "agent", "", "agent id for agent-scoped settings")
}

func loadToolProvidersExact(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ToolProviderCfg, error) {
	var out map[string]config.ToolProviderCfg
	if err := loadSettingExact(ctx, st, userID, agentID, gateway.NSToolProviders, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]config.ToolProviderCfg{}
	}
	return out, nil
}

func loadToolCategoriesExact(ctx context.Context, st store.Store, userID, agentID string) (map[string]config.ToolCategoryCfg, error) {
	var out map[string]config.ToolCategoryCfg
	if err := loadSettingExact(ctx, st, userID, agentID, gateway.NSToolCategories, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]config.ToolCategoryCfg{}
	}
	return out, nil
}

func loadSettingExact(ctx context.Context, st store.Store, userID, agentID, namespace string, dst interface{}) error {
	rec, err := st.GetConfigByName(ctx, store.KindSetting, userID, agentID, namespace)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}
	blob, err := json.Marshal(rec.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, dst)
}

func saveToolProviders(ctx context.Context, st store.Store, userID, agentID string, providers map[string]config.ToolProviderCfg) error {
	return scope.SaveSetting(ctx, st, userID, agentID, gateway.NSToolProviders, structMap(providers))
}

func saveToolCategories(ctx context.Context, st store.Store, userID, agentID string, cats map[string]config.ToolCategoryCfg) error {
	return scope.SaveSetting(ctx, st, userID, agentID, gateway.NSToolCategories, structMap(cats))
}

func structMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	blob, _ := json.Marshal(v)
	var out map[string]interface{}
	_ = json.Unmarshal(blob, &out)
	return out
}
