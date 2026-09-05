package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/adapter"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/usecase"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP server authorization (OAuth)",
	}
	cmd.AddCommand(mcpLoginCmd(), mcpStatusCmd(), mcpLogoutCmd())
	return cmd
}

func mcpLoginCmd() *cobra.Command {
	var noBrowser bool
	var resource string
	cmd := &cobra.Command{
		Use:   "login <server-name>",
		Short: "Authorize an OAuth-protected MCP server (e.g. quandora)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName := args[0]
			b, closer, err := oauthBootstrapForCLI()
			if err != nil {
				return err
			}
			defer closer()
			if resource == "" {
				return fmt.Errorf("oauth resource URL is required (--oauth-resource or MCP config oauthResource)")
			}
			callbackID, err := domain.CallbackID(resource)
			if err != nil {
				return fmt.Errorf("invalid oauth resource: %w", err)
			}
			port, err := adapter.FreeLoopbackPort()
			if err != nil {
				return err
			}
			callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback/%s", port, callbackID)

			out, err := b.Start.Execute(cmd.Context(), usecase.StartAuthInput{
				UserID:      "cli",
				AgentID:     "local",
				ServerName:  serverName,
				ServerURL:   resource,
				CallbackURL: callbackURL,
			})
			if err != nil {
				return err
			}

			if noBrowser {
				fmt.Println("Open the following address in your browser. After authorizing, paste back the FULL redirected URL from the address bar:")
				fmt.Println("(A \"cannot connect\" page in the browser is expected — just copy the address bar contents and paste them here.)")
				fmt.Println()
				fmt.Println("  " + out.AuthURL)
				fmt.Println()
				fmt.Print("> ")
				reader := bufio.NewReader(os.Stdin)
				pasted, _ := reader.ReadString('\n')
				params, err := domain.ParseCallbackURL(strings.TrimSpace(pasted))
				if err != nil {
					return fmt.Errorf("parse pasted callback url: %w", err)
				}
				_, err = b.Complete.Execute(cmd.Context(), usecase.CompleteAuthInput{Callback: params})
				return err
			}

			recv, err := (&adapter.LoopbackCallbackReceiver{Port: port, CallbackID: callbackID}).Listen(cmd.Context())
			if err != nil {
				return err
			}
			if err := (&adapter.SystemBrowserOpener{}).Open(cmd.Context(), out.AuthURL); err != nil {
				fmt.Printf("Could not open the browser — open this URL manually:\n\n  %s\n", out.AuthURL)
			}
			fmt.Println("Waiting for the authorization callback…")
			params := <-recv
			if _, err := b.Complete.Execute(cmd.Context(), usecase.CompleteAuthInput{Callback: params}); err != nil {
				return err
			}
			fmt.Println("✓ Authorized")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the auth URL and paste back the redirected URL (headless/SSH)")
	cmd.Flags().StringVar(&resource, "oauth-resource", "", "OAuth resource URL (defaults to the server's oauthResource config)")
	return cmd
}

func mcpStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <server-name>",
		Short: "Show authorization status for an MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closer, err := oauthBootstrapForCLI()
			if err != nil {
				return err
			}
			defer closer()
			serverName := args[0]
			out, err := b.Status.Execute(cmd.Context(), usecase.RefreshInput{
				UserID: "cli", AgentID: "local", ServerName: serverName,
			})
			if err != nil {
				return err
			}
			fmt.Printf("status: %s\n", out.Status)
			if len(out.Scopes) > 0 {
				fmt.Printf("scopes: %s\n", strings.Join(out.Scopes, " "))
			}
			if !out.ExpiresAt.IsZero() {
				fmt.Printf("expires: %s\n", out.ExpiresAt.In(time.Local).Format(time.RFC3339))
			}
			return nil
		},
	}
	return cmd
}

func mcpLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout <server-name>",
		Short: "Revoke MCP server authorization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closer, err := oauthBootstrapForCLI()
			if err != nil {
				return err
			}
			defer closer()
			serverName := args[0]
			if err := b.Revoke.Execute(cmd.Context(), usecase.RefreshInput{
				UserID: "cli", AgentID: "local", ServerName: serverName,
			}); err != nil {
				return err
			}
			fmt.Println("✓ Authorization revoked")
			return nil
		},
	}
	return cmd
}

// oauthBootstrapForCLI initializes the OAuth bootstrap from the same env
// the daemon uses, so CLI login writes to the same (shared) credential
// store. The returned closer closes the store handle.
func oauthBootstrapForCLI() (*oauth.Bootstrap, func(), error) {
	secret := os.Getenv("FASTAGENT_OAUTH_SECRET")
	st, err := openStoreFromEnv()
	if err != nil {
		return nil, nil, err
	}
	var db *store.DBStore
	if s, ok := st.(*store.DBStore); ok {
		db = s
	}
	home, err := config.HomeDir()
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	b, err := oauth.Init(secret, oauth.Options{Home: home, DB: db})
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return b, func() { st.Close() }, nil
}
