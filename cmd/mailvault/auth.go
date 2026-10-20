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
		Short: "Authorize a configured mail source",
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
			authConfig := auth.Config{Provider: oauth.Provider, ClientID: oauth.ClientID, Tenant: oauth.Tenant, Scopes: oauth.Scopes,
				DeviceCodeURL: oauth.DeviceCodeURL, AuthorizationURL: oauth.AuthorizationURL, TokenURL: oauth.TokenURL}
			store := auth.Store(auth.KeyringStore{})
			if oauth.TokenFile != "" {
				store = auth.FileStore{Path: oauth.TokenFile}
			}
			if oauth.Provider == "google" {
				return auth.LoopbackLogin(cmd.Context(), authConfig, config.OAuthTokenAccount(source), store, cmd.OutOrStdout())
			}
			return auth.DeviceLogin(cmd.Context(), authConfig, config.OAuthTokenAccount(source), store, cmd.OutOrStdout())
		},
	}
	login.Flags().StringVar(&sourceName, "source", "", "configured OAuth2 source name")
	cmd.AddCommand(login)
	return cmd
}
