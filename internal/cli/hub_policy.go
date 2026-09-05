package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubPolicyCommand(lookupEnv func(string) string) *cobra.Command {
	var configPath, projectID, expectedID, tokenEnv string
	cmd := &cobra.Command{Use: "policy", Short: "Inspect and explicitly approve repository execution policy", Example: "detent hub policy inspect --project orders", Args: NoArgs}
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "Customer global configuration path")
	cmd.PersistentFlags().StringVar(&projectID, "project", "", "Configured project ID")
	inspect := &cobra.Command{
		Use: "inspect", Short: "Print the resolved descriptor without uploading private configuration", Args: NoArgs,
		Example: "detent hub policy inspect --config /etc/detent/config.yaml --project orders",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _, descriptor, err := resolveHubPolicy(cmd.Context(), configPath, projectID)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(descriptor)
		},
	}
	approve := &cobra.Command{
		Use: "approve", Short: "Approve the resolved repository policy using an administrator credential", Args: NoArgs,
		Example: "detent hub policy approve --config /etc/detent/config.yaml --project orders",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, repository, descriptor, err := resolveHubPolicy(cmd.Context(), configPath, projectID)
			if err != nil {
				return err
			}
			client, err := hubclient.New(hubclient.Config{URL: cfg.Client.URL, TokenSource: func() string { return lookupEnv(tokenEnv) }})
			if err != nil {
				return err
			}
			change := policy.Change{ExpectedID: expectedID, Policy: descriptor}
			var approval policy.Approval
			if id := cfg.Client.NativeProjects[projectID]; id != "" {
				native, nativeErr := client.Native(tracker.OrganizationID(cfg.Client.OrganizationID), tracker.ProjectID(id))
				if nativeErr != nil {
					return nativeErr
				}
				approval, err = native.ApproveProjectPolicy(cmd.Context(), change)
			} else {
				approval, err = client.ApproveProjectPolicy(cmd.Context(), repository, change)
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(approval)
		},
	}
	approve.Flags().StringVar(&expectedID, "expected-policy-id", "", "Currently approved policy ID; empty for first approval")
	approve.Flags().StringVar(&tokenEnv, "admin-token-env", "DETENT_HUB_ADMIN_TOKEN", "Environment variable containing the Hub administrator token")
	cmd.AddCommand(inspect, approve)
	return cmd
}

func resolveHubPolicy(ctx context.Context, configPath, projectID string) (globalconfig.Config, string, policy.Descriptor, error) {
	resolution, err := globalconfig.ResolvePath(configPath)
	if err != nil {
		return globalconfig.Config{}, "", policy.Descriptor{}, err
	}
	cfg, err := globalconfig.Read(resolution.Path)
	if err != nil {
		return cfg, "", policy.Descriptor{}, err
	}
	for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
		if selected.ID != projectID {
			continue
		}
		workflow, err := project.LoadWorkflowContext(ctx, selected)
		if err != nil {
			return cfg, "", policy.Descriptor{}, err
		}
		descriptor, err := project.ResolvePolicy(selected, workflow)
		return cfg, workflow.Config.Tracker.Repository, descriptor, err
	}
	return cfg, "", policy.Descriptor{}, fmt.Errorf("configured project %q was not found", projectID)
}
