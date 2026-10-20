package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
)

func init() { rootCmd.AddCommand(newListFoldersCommand()) }

func newListFoldersCommand() *cobra.Command {
	var sourceName string
	cmd := &cobra.Command{
		Use:   "list-folders",
		Short: "List selectable source folders as configuration-ready YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if sourceName == "" {
				return errors.New("--source is required")
			}
			cfg, err := config.LoadSelected(cfgFile, sourceName, "")
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			var selected *config.SourceConfig
			for i := range cfg.Sources {
				if cfg.Sources[i].Name == sourceName {
					selected = &cfg.Sources[i]
					break
				}
			}
			if selected == nil {
				return fmt.Errorf("unknown source %q", sourceName)
			}
			values, err := config.SourcePluginConfig(*selected)
			if err != nil {
				return err
			}
			source, err := builtin.Registry().CreateSource(selected.Type, values)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, source.Close()) }()
			if err = source.Connect(cmd.Context()); err != nil {
				return fmt.Errorf("connect source %q: %w", sourceName, err)
			}
			folders, err := source.ListFolders(cmd.Context())
			if err != nil {
				return fmt.Errorf("list source %q folders: %w", sourceName, err)
			}
			sort.Strings(folders)
			if folders == nil {
				folders = []string{}
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(folders)
			}
			if len(folders) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "folders: []")
				return err
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), "folders:"); err != nil {
				return err
			}
			for _, folder := range folders {
				encoded, marshalErr := json.Marshal(folder)
				if marshalErr != nil {
					return marshalErr
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", encoded); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceName, "source", "", "configured source name")
	return cmd
}
