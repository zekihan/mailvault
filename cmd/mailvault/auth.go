package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/auth"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
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
				DeviceCodeURL: oauth.DeviceCodeURL, AuthorizationURL: oauth.AuthorizationURL, TokenURL: oauth.TokenURL,
				ValidateAccessToken: mailboxTokenValidator(source, builtin.Registry())}
			store := auth.Store(auth.KeyringStore{})
			if oauth.TokenFile != "" {
				store = auth.FileStore{Path: oauth.TokenFile}
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Sign in with the account that owns mailbox %q (source %q). Mailbox access will be checked before credentials are saved.\n", source.Username, source.Name); err != nil {
				return err
			}
			if oauth.Provider == "google" {
				err = auth.LoopbackLogin(cmd.Context(), authConfig, config.OAuthTokenAccount(source), store, cmd.OutOrStdout())
			} else {
				err = auth.DeviceLogin(cmd.Context(), authConfig, config.OAuthTokenAccount(source), store, cmd.OutOrStdout())
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Mailbox access verified for source %q; OAuth credentials saved.\n", source.Name)
			return err
		},
	}
	login.Flags().StringVar(&sourceName, "source", "", "configured OAuth2 source name")
	cmd.AddCommand(login)
	return cmd
}

func mailboxTokenValidator(source config.SourceConfig, registry *plugins.Registry) func(context.Context, string) error {
	return func(ctx context.Context, token string) (err error) {
		values, err := config.SourcePluginConfig(source)
		if err != nil {
			return err
		}
		values["oauth2_token_provider"] = func(context.Context) (string, error) { return token, nil }
		client, err := registry.CreateSource(source.Type, values)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, client.Close()) }()
		if err := client.Connect(ctx); err != nil {
			return fmt.Errorf("check mailbox %q for source %q: %w", source.Username, source.Name, err)
		}
		return nil
	}
}
