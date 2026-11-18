package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

const hubSecretsRewrapProcedure = "the Hub rewraps provider secrets automatically after restart. Add the new key while retaining old versions, set it active, restart the Hub, then read GET /api/v2/organizations/{organization}/secrets/status until it reports no problems before retiring old keys. See docs/hub-self-hosting.md"

func newHubSecretsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secrets",
		Short:   "Maintain encrypted Hub provider secrets",
		Example: "detent hub secrets",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return errors.New(hubSecretsRewrapProcedure)
			}
			return nil
		},
	}
	cmd.FParseErrWhitelist.UnknownFlags = true
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		_, err := cmd.OutOrStdout().Write([]byte(hubSecretsRewrapProcedure + "\n"))
		return err
	}
	return cmd
}
