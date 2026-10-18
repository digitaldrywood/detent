package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubIssueCommand(lookupEnv func(string) string) *cobra.Command {
	var configPath, projectID, hubURL, organization, identityFile, tokenEnv string
	var sourceProject, storeID, sourceKind string
	var dryRun bool
	var laneMap map[string]string
	var terminalStates []string
	root := &cobra.Command{
		Use:     "issue",
		Short:   "Migrate a retired local board into the Hub",
		Example: "detent hub issue import-local board.sqlite --project prj_example --store-id example-board --dry-run",
		Args:    NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "Customer global configuration path")
	root.PersistentFlags().StringVar(&projectID, "project", "", "Configured project name or native project ID")
	root.PersistentFlags().StringVar(&hubURL, "hub-url", "", "Explicit Hub URL; otherwise use customer configuration")
	root.PersistentFlags().StringVar(&organization, "organization", "", "Native organization ID")
	root.PersistentFlags().StringVar(&identityFile, "identity-file", "", "Enrolled runner identity file")
	root.PersistentFlags().StringVar(&tokenEnv, "token-env", "DETENT_HUB_TOKEN", "Environment variable containing a scoped Hub credential")
	command := &cobra.Command{
		Use:     "import-local <sqlite-copy>",
		Short:   "Import a retired local board copy into a native project",
		Example: "detent hub issue import-local board.sqlite --project prj_example --source-project default --kind local_sqlite --store-id example-board --dry-run",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := hubclient.Config{URL: hubURL, IdentityFile: identityFile, TokenSource: func() string { return lookupEnv(tokenEnv) }}
			org, project := organization, projectID
			if cfg.URL == "" {
				resolution, err := globalconfig.ResolvePath(configPath)
				if err != nil {
					return err
				}
				settings, err := globalconfig.Read(resolution.Path)
				if err != nil {
					return err
				}
				cfg.URL, cfg.IdentityFile = settings.Client.URL, settings.Client.IdentityFile
				org = settings.Client.OrganizationID
				if id := settings.Client.NativeProjects[projectID]; id != "" {
					project = id
				}
			}
			client, err := hubclient.New(cfg)
			if err != nil {
				return err
			}
			native, err := client.Native(tracker.OrganizationID(org), tracker.ProjectID(project))
			if err != nil {
				return err
			}
			records, err := localboard.Read(cmd.Context(), args[0], sourceProject, sourceKind, storeID, terminalStates)
			if err != nil {
				return err
			}
			for i := range records {
				if state, ok := laneMap[records[i].State]; ok {
					records[i].State = state
				}
			}
			value, err := importLocalBoardRecords(cmd.Context(), native, records, dryRun)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
		},
	}
	command.Flags().StringVar(&sourceProject, "source-project", "default", "Project ID inside the source SQLite store")
	command.Flags().StringVar(&storeID, "store-id", "", "Stable name for this source store; reuse on every import")
	command.Flags().StringVar(&sourceKind, "kind", "local_sqlite", "Source tracker: local_sqlite or github_local")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Report the import without writing native records")
	command.Flags().StringToStringVar(&laneMap, "lane-map", nil, "Source=target lane mappings")
	command.Flags().StringSliceVar(&terminalStates, "terminal-state", []string{"Done", "Cancelled", "Closed"}, "Source terminal lanes; imported only when needed as prerequisites")
	root.AddCommand(command)
	return root
}
