package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/auth"
	"github.com/zekihan/mailvault/internal/config"
)

func init() {
	rootCmd.AddCommand(newAuthCommand())
}

func newAuthCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Manage source authentication"}
	var sourceName string
	login := &cobra.Command{
		Use:   "login",
		Short: "Authorize a Microsoft mail source with device login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if sourceName == "" {
				return fmt.Errorf("--source is required")
			}
			source, err := config.LoadOAuthSource(cfgFile, sourceName)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			oauth := source.OAuth2
			return auth.DeviceLogin(cmd.Context(), auth.Config{
				ClientID: oauth.ClientID, Tenant: oauth.Tenant, Scopes: oauth.Scopes,
				DeviceCodeURL: oauth.DeviceCodeURL, TokenURL: oauth.TokenURL,
			}, config.OAuthTokenAccount(source), auth.KeyringStore{}, cmd.OutOrStdout())
		},
	}
	login.Flags().StringVar(&sourceName, "source", "", "configured OAuth2 source name")
	cmd.AddCommand(login)
	return cmd
}
