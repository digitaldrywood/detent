package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	"github.com/digitaldrywood/detent/internal/connector/local"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func newWorkItemCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "work-item",
		Short:   "Manage runtime work items",
		Example: `detent work-item add digitaldrywood-video --title "Author beat visuals" --body-file brief.md`,
	}
	cmd.AddCommand(newWorkItemAddCommand(configPath, opts))
	return cmd
}

func newWorkItemAddCommand(configPath *string, opts options) *cobra.Command {
	var title string
	var body string
	var bodyFile string
	var state string
	var labels []string
	var fields []string
	var priority int
	var deliverableReviewURL string
	cmd := &cobra.Command{
		Use:     "add PROJECT_ID",
		Short:   "Create a runtime work item",
		Example: `detent work-item add digitaldrywood-video --title "Author beat visuals" --body-file brief.md --label video-assets --field render_status=queued`,
		Args: func(cmd *cobra.Command, args []string) error {
			return WrapValidation(cobra.ExactArgs(1)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			description, err := workItemBody(body, bodyFile)
			if err != nil {
				return err
			}
			parsedFields, err := workItemFields(fields)
			if err != nil {
				return err
			}
			req := workitem.Request{
				Title:       title,
				Description: description,
				State:       state,
				Labels:      labels,
				Fields:      parsedFields,
			}
			if flagChanged(cmd, "priority") {
				req.Priority = &priority
			}
			if deliverableReviewURL = strings.TrimSpace(deliverableReviewURL); deliverableReviewURL != "" {
				req.Deliverable = &connector.Deliverable{
					Kind:      workflowconfig.DeliverableArtifact,
					ReviewURL: deliverableReviewURL,
				}
			}
			result, err := runWorkItemAdd(cmd.Context(), *configPath, args[0], req, opts)
			if err != nil {
				return cliWorkItemError(err)
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "created work item %s\n", result.Identifier)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "work item title")
	cmd.Flags().StringVar(&body, "body", "", "work item description")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "path to a markdown description")
	cmd.Flags().StringVar(&state, "state", "", "initial tracker state")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "label to attach; repeat for multiple labels")
	cmd.Flags().StringArrayVar(&fields, "field", nil, "field assignment key=value; repeat for multiple fields")
	cmd.Flags().IntVar(&priority, "priority", 0, "work item priority")
	cmd.Flags().StringVar(&deliverableReviewURL, "deliverable-review-url", "", "deliverable review URL")
	return cmd
}

func runWorkItemAdd(ctx context.Context, configPath string, projectID string, req workitem.Request, opts options) (result workitem.Response, err error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return result, err
	}
	cfg, _, err := opts.readProject(resolution.Path, projectID)
	if err != nil {
		return result, err
	}
	if len(cfg.Projects) == 0 {
		return result, fmt.Errorf("%w: %s", ErrProjectNotFound, projectID)
	}
	projectConfig := cfg.Projects[0]
	workflow, err := projectpkg.LoadWorkflowContext(ctx, projectConfig)
	if err != nil {
		return result, err
	}
	workflow.Config = workItemWorkflowConfig(projectConfig, workflow.Config)
	workflow.Config, err = githubLocalWorkflowConfigWithGlobalToken(ctx, cfg, workflow.Config, opts)
	if err != nil {
		return result, err
	}
	if err := workflow.Config.Validate(); err != nil {
		return result, err
	}
	if !workitem.SupportsTracker(workflow.Config.Tracker.Kind) {
		return workitem.Create(ctx, workitem.Target{
			ProjectID: projectConfig.ID,
			Workflow:  workflow.Config,
		}, req)
	}

	conn, err := workItemConnectorFromWorkflow(ctx, workflow.Config)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := closeWorkItemConnector(conn); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	return workitem.Create(ctx, workitem.Target{
		ProjectID: projectConfig.ID,
		Workflow:  workflow.Config,
		Connector: conn,
	}, req)
}

func workItemWorkflowConfig(project globalconfig.Project, cfg workflowconfig.Config) workflowconfig.Config {
	workdir := strings.TrimSpace(project.Workdir)
	if workdir == "" {
		return cfg
	}
	switch cfg.Tracker.Kind {
	case workflowconfig.TrackerLocalSQLite, workflowconfig.TrackerGitHubLocal:
		cfg.Tracker.LocalSQLite.Path = githubLocalProjectRelativePath(workdir, cfg.Tracker.LocalSQLite.Path)
	}
	return cfg
}

func workItemConnectorFromWorkflow(ctx context.Context, cfg workflowconfig.Config) (connector.Connector, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := factory.NewFromConfig(factory.Config{
		Kind: cfg.Tracker.Kind,
		LocalSQLite: local.Config{
			Path:           cfg.Tracker.LocalSQLite.Path,
			ProjectID:      cfg.Tracker.LocalSQLite.ProjectID,
			Issues:         cfg.Tracker.Issues,
			ActiveStates:   cfg.Tracker.ActiveStates,
			ObservedStates: cfg.Tracker.ObservedStates,
			TerminalStates: cfg.Tracker.TerminalStates,
		},
		Endpoint:                    cfg.Tracker.Endpoint,
		APIKey:                      cfg.Tracker.APIKey,
		HTTPMaxIdleConns:            cfg.Tracker.HTTPMaxIdleConns,
		HTTPMaxIdleConnsPerHost:     cfg.Tracker.HTTPMaxIdleConnsPerHost,
		HTTPIdleConnTimeoutMS:       cfg.Tracker.HTTPIdleConnTimeoutMS,
		GitHubRESTMinReserve:        cfg.Tracker.GitHubRESTMinReserve,
		GitHubRESTFanoutMaxRequests: cfg.Tracker.GitHubRESTFanoutMaxRequests,
		GitHubRESTDebugLogging:      cfg.Tracker.GitHubRESTDebugLogging,
		GitHubAppID:                 cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:         cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath:     cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID:     cfg.Tracker.GitHubAppInstallationID,
		GitHubStatusSource:          cfg.Tracker.GitHubStatusSource,
		Repository:                  cfg.Tracker.Repository,
		StatusField:                 cfg.Tracker.StatusField,
		StatusLabelPrefix:           cfg.Tracker.StatusLabelPrefix,
		ActiveStates:                cfg.Tracker.ActiveStates,
		ObservedStates:              cfg.Tracker.ObservedStates,
		TerminalStates:              cfg.Tracker.TerminalStates,
		StateMap:                    githubLocalTrackerStateMap(cfg.Tracker.StateMap),
		PriorityMap:                 githubLocalTrackerPriorityMap(cfg.Tracker.PriorityMap),
		RequiredStatusChecks:        cfg.Gate.RequiredStatusChecks,
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, closeWorkItemConnector(conn))
	}
	return conn, nil
}

func closeWorkItemConnector(conn connector.Connector) error {
	if closer, ok := conn.(connector.Closer); ok {
		return closer.Close()
	}
	return nil
}

func workItemBody(body string, bodyFile string) (string, error) {
	body = strings.TrimSpace(body)
	bodyFile = strings.TrimSpace(bodyFile)
	if body != "" && bodyFile != "" {
		return "", WrapValidation(errors.New("--body and --body-file cannot both be set"))
	}
	if bodyFile != "" {
		raw, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("read body file: %w", err)
		}
		body = strings.TrimSpace(string(raw))
	}
	if body == "" {
		return "", WrapValidation(errors.New("--body or --body-file is required"))
	}
	return body, nil
}

func workItemFields(values []string) (map[string]string, error) {
	fields := map[string]string{}
	for _, value := range values {
		key, fieldValue, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, WrapValidation(fmt.Errorf("field %q must use key=value", value))
		}
		fields[key] = strings.TrimSpace(fieldValue)
	}
	return fields, nil
}

func cliWorkItemError(err error) error {
	var itemErr *workitem.Error
	if !errors.As(err, &itemErr) {
		return err
	}
	switch itemErr.Code {
	case workitem.CodeMissingTitle,
		workitem.CodeMissingDescription,
		workitem.CodeInvalidState,
		workitem.CodeInvalidFields:
		return WrapValidation(itemErr)
	default:
		return itemErr
	}
}
