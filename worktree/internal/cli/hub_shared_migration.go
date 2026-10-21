package cli

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func newHubSharedMigrationCommand(lookupEnv func(string) string) *cobra.Command {
	var databasePath, hostedConfigPath string
	cmd := &cobra.Command{
		Use:          "migrate-shared-origin",
		Short:        "Move a stopped origin-bound hosted Hub to the shared entry origin",
		Long:         "Offline, versioned migration of a stopped hosted Hub from its dedicated public origin to the shared entry origin. It preserves organization, provider, membership, project, runner, history and billing records, advances the allocation generation, and revokes old browser sessions, login transactions and outstanding artifact grants. Rerunning with the same target is a no-op.",
		Example:      "detent hub migrate-shared-origin --database /var/lib/detent/org.db --hosted-config /etc/detent/org-shared.yaml",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := OutputForCommand(cmd); err != nil {
				return err
			}
			if strings.TrimSpace(databasePath) == "" || strings.TrimSpace(hostedConfigPath) == "" {
				return errors.New("hub migrate-shared-origin requires --database and --hosted-config")
			}
			hosted, _, err := readHostedConfig(hostedConfigPath, lookupEnv)
			if err != nil {
				return err
			}
			result, err := hubserver.MigrateHostedSharedOrigin(cmd.Context(), hubserver.Config{DatabasePath: databasePath, Hosted: hosted, GitHubDisabled: true})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
	cmd.Flags().StringVar(&databasePath, "database", "", "stopped origin-bound hosted Hub database")
	cmd.Flags().StringVar(&hostedConfigPath, "hosted-config", "", "target hosted configuration containing shared_entry and the shared public_url")
	return cmd
}
