package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
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
			cfg, workflow, descriptor, err := resolveHubPolicy(cmd.Context(), configPath, projectID)
			if err != nil {
				return err
			}
			settings := cfg.Client.Normalized()
			if id := settings.NativeProjects[projectID]; id != "" && (settings.IdentityFile != "" || strings.TrimSpace(lookupEnv(settings.TokenEnvironment)) != "") {
				client, err := hubclient.New(hubclient.Config{URL: settings.URL, IdentityFile: settings.IdentityFile, TokenSource: func() string { return lookupEnv(settings.TokenEnvironment) }, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
				if err != nil {
					return err
				}
				native, err := client.Native(tracker.OrganizationID(settings.OrganizationID), tracker.ProjectID(id))
				if err != nil {
					return err
				}
				workflow, err = native.ResolveProjectWorkflow(cmd.Context(), workflow)
				if err != nil {
					return err
				}
				descriptor, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					return err
				}
			}
			if steps := nativeWorkflowGitHubSteps(workflow.Config, workflow.Prompt); len(steps) > 0 {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+nativeWorkflowGitHubStepsWarning(steps)); err != nil {
					return err
				}
			}
			for _, warning := range nativeCompletionWorkflowWarnings(workflow.Config, descriptor) {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+warning); err != nil {
					return err
				}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(descriptor)
		},
	}
	approve := &cobra.Command{
		Use: "approve", Short: "Approve the resolved repository policy using an administrator credential", Args: NoArgs,
		Example: "detent hub policy approve --config /etc/detent/config.yaml --project orders",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, workflow, descriptor, err := resolveHubPolicy(cmd.Context(), configPath, projectID)
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
				approval, err = client.ApproveProjectPolicy(cmd.Context(), workflow.Config.Tracker.Repository, change)
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

func nativeCompletionWorkflowWarnings(cfg workflowconfig.Config, descriptor policy.Descriptor) []string {
	if cfg.Tracker.Kind != workflowconfig.TrackerHubNative {
		return nil
	}
	lanes := cfg.NativeWorkflowStates()
	if descriptor.Workflow != nil {
		lanes = descriptor.Workflow.States
	}
	states := make([]connector.WorkflowState, len(lanes))
	for i, lane := range lanes {
		states[i] = connector.WorkflowState{Name: lane.Name, Terminal: lane.Terminal, Dispatchable: lane.Dispatchable, OperatorOnly: lane.OperatorOnly, Transitions: lane.Transitions}
	}
	var warnings []string
	for _, lane := range states {
		if lane.Terminal || !lane.Dispatchable || lane.OperatorOnly {
			continue
		}
		_, landing := connector.CompletionLane(states, lane.Name, "", false)
		if merging, ok := connector.CompletionLane(states, lane.Name, "Merging", true); ok {
			for _, candidate := range states {
				if candidate.Name == merging && candidate.Dispatchable {
					_, landing = connector.CompletionLane(states, merging, "", false)
				}
			}
		}
		if !landing {
			warnings = append(warnings, fmt.Sprintf("native workflow has no landing path from %s to a terminal lane", lane.Name))
		}
		if _, ok := connector.LandingRefusalLane(states, lane.Name, cfg.Agent.AutoPromote.SourceState, false); !ok {
			warnings = append(warnings, "native workflow has no park lane reachable from "+lane.Name)
		}
	}
	return warnings
}

func resolveHubPolicy(ctx context.Context, configPath, projectID string) (globalconfig.Config, workflowconfig.Workflow, policy.Descriptor, error) {
	resolution, err := globalconfig.ResolvePath(configPath)
	if err != nil {
		return globalconfig.Config{}, workflowconfig.Workflow{}, policy.Descriptor{}, err
	}
	cfg, err := globalconfig.Read(resolution.Path)
	if err != nil {
		return cfg, workflowconfig.Workflow{}, policy.Descriptor{}, err
	}
	for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
		if selected.ID != projectID {
			continue
		}
		workflow, err := project.LoadWorkflowContext(ctx, selected)
		if err != nil {
			return cfg, workflowconfig.Workflow{}, policy.Descriptor{}, err
		}
		workflow.Config = project.MapNativeTracker(workflow.Config, cfg.Client.NativeProjects[selected.ID] != "")
		descriptor, err := project.ResolvePolicy(selected, workflow)
		return cfg, workflow, descriptor, err
	}
	return cfg, workflowconfig.Workflow{}, policy.Descriptor{}, fmt.Errorf("configured project %q was not found", projectID)
}
