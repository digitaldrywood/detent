package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/factory"
	"github.com/digitaldrywood/detent/internal/connector/local"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

type githubLocalImportResult struct {
	Status   string                     `json:"status"`
	Project  string                     `json:"project"`
	Imported []githubLocalImportedIssue `json:"imported"`
}

type githubLocalImportedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url"`
}

type githubLocalImporter interface {
	ImportIssues(context.Context, []int, string) ([]connector.Issue, error)
	Close() error
}

func newGitHubLocalCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "github-local",
		Short:   "Manage github_local tracker state",
		Example: "detent github-local import detent 779 --state Todo",
	}
	cmd.AddCommand(newGitHubLocalImportCommand(configPath, opts))
	return cmd
}

func newGitHubLocalImportCommand(configPath *string, opts options) *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:     "import PROJECT_ID ISSUE_NUMBER...",
		Short:   "Import explicit GitHub issues into local tracker state",
		Example: "detent github-local import detent 779 780,781 --state Todo",
		Args: func(cmd *cobra.Command, args []string) error {
			return WrapValidation(cobra.MinimumNArgs(2)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			numbers, err := parseGitHubLocalIssueNumbers(args[1:])
			if err != nil {
				return err
			}
			result, err := runGitHubLocalImport(cmd.Context(), *configPath, args[0], numbers, state, opts)
			if err != nil {
				return err
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "imported %d issue(s) into %s\n", len(result.Imported), result.Project)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "local Detent state to assign to imported issues")
	return cmd
}

func runGitHubLocalImport(ctx context.Context, configPath string, projectID string, numbers []int, state string, opts options) (result githubLocalImportResult, err error) {
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
	project := cfg.Projects[0]
	workflow, err := projectpkg.LoadWorkflowContext(ctx, project)
	if err != nil {
		return result, err
	}
	workflow.Config = githubLocalWorkflowConfig(project, workflow.Config)
	workflow.Config, err = githubLocalWorkflowConfigWithGlobalToken(ctx, cfg, workflow.Config, opts)
	if err != nil {
		return result, err
	}
	if workflow.Config.Tracker.Kind != workflowconfig.TrackerGitHubLocal {
		return result, fmt.Errorf("project %q uses tracker.kind %q, want github_local", project.ID, workflow.Config.Tracker.Kind)
	}
	if err := workflow.Config.Validate(); err != nil {
		return result, err
	}

	conn, err := githubLocalConnectorFromWorkflow(ctx, workflow.Config)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	imported, err := conn.ImportIssues(ctx, numbers, state)
	if err != nil {
		return result, err
	}
	return githubLocalImportResult{
		Status:   "ok",
		Project:  project.ID,
		Imported: githubLocalImportedIssues(imported),
	}, nil
}

func parseGitHubLocalIssueNumbers(values []string) ([]int, error) {
	out := []int{}
	seen := map[int]struct{}{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			number, err := strconv.Atoi(part)
			if err != nil || number <= 0 {
				return nil, WrapValidation(fmt.Errorf("issue number %q must be a positive integer", part))
			}
			if _, ok := seen[number]; ok {
				continue
			}
			seen[number] = struct{}{}
			out = append(out, number)
		}
	}
	if len(out) == 0 {
		return nil, WrapValidation(errors.New("at least one issue number is required"))
	}
	return out, nil
}

func githubLocalConnectorFromWorkflow(ctx context.Context, cfg workflowconfig.Config) (githubLocalImporter, error) {
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
		if closer, ok := conn.(connector.Closer); ok {
			return nil, errors.Join(err, closer.Close())
		}
		return nil, err
	}
	importer, ok := conn.(githubLocalImporter)
	if !ok {
		typeErr := fmt.Errorf("connector %T does not support github_local import", conn)
		if closer, ok := conn.(connector.Closer); ok {
			return nil, errors.Join(typeErr, closer.Close())
		}
		return nil, typeErr
	}
	return importer, nil
}

func githubLocalWorkflowConfig(project globalconfig.Project, cfg workflowconfig.Config) workflowconfig.Config {
	workdir := strings.TrimSpace(project.Workdir)
	if workdir == "" {
		return cfg
	}
	if cfg.Tracker.Kind == workflowconfig.TrackerGitHubLocal {
		cfg.Tracker.LocalSQLite.Path = githubLocalProjectRelativePath(workdir, cfg.Tracker.LocalSQLite.Path)
	}
	return cfg
}

func githubLocalWorkflowConfigWithGlobalToken(ctx context.Context, global globalconfig.Config, cfg workflowconfig.Config, opts options) (workflowconfig.Config, error) {
	if strings.TrimSpace(cfg.Tracker.APIKey) != "" || strings.TrimSpace(global.GitHubToken) == "" {
		return cfg, nil
	}
	token, err := resolveConfiguredGitHubToken(ctx, global.GitHubToken, runtimeDeps{
		lookupEnv:   opts.lookupEnv,
		ghAuthToken: opts.ghAuthToken,
	})
	if err != nil {
		return cfg, err
	}
	cfg.Tracker.APIKey = token.Value
	return cfg, nil
}

func githubLocalProjectRelativePath(base string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~/") {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}

func githubLocalImportedIssues(issues []connector.Issue) []githubLocalImportedIssue {
	out := make([]githubLocalImportedIssue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, githubLocalImportedIssue{
			ID:         issue.ID,
			Identifier: issue.Identifier,
			Title:      issue.Title,
			State:      issue.State,
			URL:        issue.URL,
		})
	}
	return out
}

func githubLocalTrackerStateMap(value workflowconfig.StringOrMap) map[string]string {
	if !value.IsMap {
		return nil
	}
	out := make(map[string]string, len(value.Map))
	for state, mapped := range value.Map {
		mappedState, ok := mapped.(string)
		if !ok {
			continue
		}
		state = strings.TrimSpace(state)
		mappedState = strings.TrimSpace(mappedState)
		if state != "" && mappedState != "" {
			out[state] = mappedState
		}
	}
	return out
}

func githubLocalTrackerPriorityMap(value workflowconfig.StringOrMap) map[string]*int {
	if !value.IsMap {
		return nil
	}
	out := make(map[string]*int, len(value.Map))
	for name, rank := range value.Map {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		switch rank := rank.(type) {
		case nil:
			out[name] = nil
		case int:
			rankValue := rank
			out[name] = &rankValue
		}
	}
	return out
}
