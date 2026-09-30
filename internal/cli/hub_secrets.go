package cli

import (
	"encoding/json"
	"errors"
	"os/user"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/spf13/cobra"
)

func newHubSecretsCommand(lookupEnv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use: "secrets", Short: "Maintain encrypted Hub provider secrets", Args: NoArgs,
		Example: "detent hub secrets rotate --database /var/lib/detent/hub.db",
	}
	var databasePath string
	rotate := &cobra.Command{
		Use: "rotate", Short: "Rewrap data keys in a stopped Hub using the active environment key version", Args: NoArgs, SilenceUsage: true,
		Example: "detent hub secrets rotate --database /var/lib/detent/hub.db",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(databasePath) == "" {
				return errors.New("hub secrets rotate requires --database")
			}
			keys, err := hubsecrets.FromEnvironment(lookupEnv)
			if err != nil {
				return err
			}
			if keys == nil {
				return hubsecrets.ErrUnavailable
			}
			actor, err := user.Current()
			if err != nil {
				return errors.New("resolve operator identity for secret rotation audit")
			}
			count, err := hubserver.RotateProjectSecrets(cmd.Context(), hubserver.Config{DatabasePath: databasePath, SecretKeys: keys}, "operator:"+actor.Username)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Rotated    int `json:"rotated"`
				KeyVersion int `json:"key_version"`
			}{count, keys.Version()})
		},
	}
	rotate.Flags().StringVar(&databasePath, "database", "", "stopped Hub database to rotate")
	cmd.AddCommand(rotate)
	return cmd
}
