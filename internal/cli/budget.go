package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type budgetOverrideResult struct {
	ProjectID      string    `json:"project_id"`
	PerDayMaxUSD   *float64  `json:"per_day_max_usd"`
	PerIssueMaxUSD *float64  `json:"per_issue_max_usd"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	Reason         string    `json:"reason"`
	Remaining      string    `json:"remaining"`
}

type budgetOverrideClearedResult struct {
	Status    string `json:"status"`
	ProjectID string `json:"project_id"`
}

func newBudgetCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "budget",
		Short:   "Manage temporary budget overrides",
		Example: "detent budget overrides",
	}
	override := newBudgetOverrideCommand(configPath, opts)
	override.AddCommand(newBudgetOverrideClearCommand(configPath, opts))
	cmd.AddCommand(override, newBudgetOverridesCommand(configPath, opts))
	return cmd
}

func newBudgetOverrideCommand(configPath *string, opts options) *cobra.Command {
	var projectID string
	var perDay float64
	var perIssue float64
	var duration time.Duration
	var reason string
	cmd := &cobra.Command{
		Use:     "override",
		Short:   "Set a self-expiring budget override",
		Example: "detent budget override --project api --per-day-max-usd 150 --duration 4h --reason 'release work'",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			var dayCap, issueCap *float64
			if flagChanged(cmd, "per-day-max-usd") {
				dayCap = &perDay
			}
			if flagChanged(cmd, "per-issue-max-usd") {
				issueCap = &perIssue
			}
			writer, workflow, closeStore, err := openBudgetOverrideContext(cmd.Context(), *configPath, projectID, opts)
			if err != nil {
				return err
			}
			defer closeStore()
			now := time.Now().UTC().Truncate(time.Second)
			override, err := budget.SetOverride(cmd.Context(), writer, budgetConfig(projectID, workflow.Config.Budget, writer), budgetLimits(workflow.Config.Budget), budget.OverrideRequest{
				ProjectID:      projectID,
				PerDayMaxUSD:   dayCap,
				PerIssueMaxUSD: issueCap,
				Duration:       duration,
				Reason:         reason,
				Now:            now,
			})
			if err != nil {
				return WrapValidation(err)
			}
			return writeBudgetOverride(out, override, now)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "project id")
	cmd.Flags().Float64Var(&perDay, "per-day-max-usd", 0, "temporary daily cap in USD")
	cmd.Flags().Float64Var(&perIssue, "per-issue-max-usd", 0, "temporary per-issue cap in USD")
	cmd.Flags().DurationVar(&duration, "duration", 0, "override lifetime")
	cmd.Flags().StringVar(&reason, "reason", "", "audit reason for the override")
	markFlagRequired(cmd, "project")
	markFlagRequired(cmd, "duration")
	markFlagRequired(cmd, "reason")
	return cmd
}

func newBudgetOverridesCommand(configPath *string, opts options) *cobra.Command {
	return &cobra.Command{
		Use:     "overrides",
		Short:   "List active budget overrides",
		Example: "detent budget overrides",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			writer, closeStore, err := openBudgetOverrideStore(cmd.Context(), *configPath, opts)
			if err != nil {
				return err
			}
			defer closeStore()
			now := time.Now().UTC().Truncate(time.Second)
			overrides, err := writer.ListActiveBudgetOverrides(cmd.Context(), now)
			if err != nil {
				return err
			}
			results := make([]budgetOverrideResult, 0, len(overrides))
			for _, override := range overrides {
				results = append(results, newBudgetOverrideResult(override, now))
			}
			return out.Write(func(out io.Writer) error {
				if len(results) == 0 {
					_, err := fmt.Fprintln(out, "No active budget overrides.")
					return err
				}
				for _, result := range results {
					if _, err := fmt.Fprintf(out, "%s: daily %s, issue %s, expires %s (%s), reason: %s\n", result.ProjectID, optionalBudgetUSD(result.PerDayMaxUSD), optionalBudgetUSD(result.PerIssueMaxUSD), result.ExpiresAt.Format(time.RFC3339), result.Remaining, result.Reason); err != nil {
						return err
					}
				}
				return nil
			}, results)
		},
	}
}

func newBudgetOverrideClearCommand(configPath *string, opts options) *cobra.Command {
	var projectID string
	cmd := &cobra.Command{
		Use:     "clear",
		Short:   "Clear a budget override before expiry",
		Example: "detent budget override clear --project api",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			writer, closeStore, err := openBudgetOverrideStore(cmd.Context(), *configPath, opts)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := writer.ClearBudgetOverride(cmd.Context(), projectID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return WrapValidation(fmt.Errorf("no active budget override for project %s", projectID))
				}
				return err
			}
			return out.Write(func(out io.Writer) error {
				_, err := fmt.Fprintf(out, "Cleared budget override for %s.\n", projectID)
				return err
			}, budgetOverrideClearedResult{Status: "ok", ProjectID: projectID})
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "project id")
	markFlagRequired(cmd, "project")
	return cmd
}

func openBudgetOverrideContext(ctx context.Context, configPath string, projectID string, opts options) (budget.OverrideWriter, workflowconfig.Workflow, func(), error) {
	owner, err := openHubBudgetOverrideStore(configPath, opts)
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, err
	}
	native, err := owner.project(projectID)
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, err
	}
	record, err := native.Project(ctx)
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, err
	}
	workflow, err := workflowconfig.ParseWorkflow([]byte(record.WorkflowMarkdown))
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, fmt.Errorf("load project workflow: %w", err)
	}
	return owner, workflow, func() {}, nil
}

func openBudgetOverrideStore(_ context.Context, configPath string, opts options) (budget.OverrideWriter, func(), error) {
	owner, err := openHubBudgetOverrideStore(configPath, opts)
	return owner, func() {}, err
}

func openHubBudgetOverrideStore(configPath string, opts options) (*hubBudgetOverrideStore, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return nil, err
	}
	settings, err := globalconfig.Read(resolution.Path)
	if err != nil {
		return nil, err
	}
	config := settings.Client.Normalized()
	if !config.Configured() || len(config.NativeProjects) == 0 {
		return nil, errors.New("budget overrides require configured Hub projects")
	}
	client, err := api.New(api.Config{URL: config.URL, IdentityFile: config.IdentityFile, TokenSource: func() string { return opts.lookupEnv(config.TokenEnvironment) }, HTTPClient: &http.Client{Transport: auditTransport{do: opts.httpDo}, Timeout: config.RequestTimeout()}})
	if err != nil {
		return nil, err
	}
	return &hubBudgetOverrideStore{client: client, organization: tracker.OrganizationID(config.OrganizationID), projects: config.NativeProjects}, nil
}

type hubBudgetOverrideStore struct {
	client       *api.Client
	organization tracker.OrganizationID
	projects     map[string]string
}

func (s *hubBudgetOverrideStore) project(id string) (*api.NativeClient, error) {
	nativeID := s.projects[strings.TrimSpace(id)]
	if nativeID == "" {
		return nil, errors.New("project is not configured for the Hub runner")
	}
	return s.client.Native(s.organization, tracker.ProjectID(nativeID))
}

func budgetOverrideHubError(err error) error {
	var failure *api.APIError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		return errors.Join(store.ErrNotFound, err)
	}
	return err
}

func (s *hubBudgetOverrideStore) ActiveBudgetOverride(ctx context.Context, id string, at time.Time) (store.BudgetOverride, error) {
	native, err := s.project(id)
	if err != nil {
		return store.BudgetOverride{}, err
	}
	return readHubBudgetOverride(ctx, native, id, at)
}

type hubBudgetSessionStore struct {
	*api.RunnerSessions
	projectID string
}

func newHubBudgetSessionStore(native *api.NativeClient, projectID string) *hubBudgetSessionStore {
	return &hubBudgetSessionStore{RunnerSessions: api.NewRunnerSessions(native), projectID: projectID}
}

func (s *hubBudgetSessionStore) ActiveBudgetOverride(ctx context.Context, id string, at time.Time) (store.BudgetOverride, error) {
	if id != s.projectID {
		return store.BudgetOverride{}, errors.New("budget project differs from the runner project")
	}
	return readHubBudgetOverride(ctx, s.NativeClient, id, at)
}

func readHubBudgetOverride(ctx context.Context, native *api.NativeClient, id string, at time.Time) (store.BudgetOverride, error) {
	var result budgetOverrideResult
	if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/budget-override", nil, &result); err != nil {
		return store.BudgetOverride{}, budgetOverrideHubError(err)
	}
	if !result.ExpiresAt.After(at) {
		return store.BudgetOverride{}, store.ErrNotFound
	}
	return store.BudgetOverride{ProjectID: id, PerDayMaxUSD: result.PerDayMaxUSD, PerIssueMaxUSD: result.PerIssueMaxUSD, CreatedAt: result.CreatedAt, ExpiresAt: result.ExpiresAt, Reason: result.Reason}, nil
}

func (s *hubBudgetOverrideStore) ListActiveBudgetOverrides(ctx context.Context, at time.Time) ([]store.BudgetOverride, error) {
	ids := make([]string, 0, len(s.projects))
	for id := range s.projects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]store.BudgetOverride, 0, len(ids))
	for _, id := range ids {
		value, err := s.ActiveBudgetOverride(ctx, id, at)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (s *hubBudgetOverrideStore) SetBudgetOverride(ctx context.Context, value store.BudgetOverride) (store.BudgetOverride, error) {
	native, err := s.project(value.ProjectID)
	if err != nil {
		return store.BudgetOverride{}, err
	}
	request := struct {
		tracker.Mutation
		PerDayMaxUSD   *float64  `json:"per_day_max_usd"`
		PerIssueMaxUSD *float64  `json:"per_issue_max_usd"`
		ExpiresAt      time.Time `json:"expires_at"`
		Reason         string    `json:"reason"`
	}{tracker.Mutation{IdempotencyKey: rand.Text()}, value.PerDayMaxUSD, value.PerIssueMaxUSD, value.ExpiresAt, value.Reason}
	var result budgetOverrideResult
	if err := native.Transport().Request(ctx, http.MethodPut, native.Scope()+"/budget-override", request, &result); err != nil {
		return store.BudgetOverride{}, budgetOverrideHubError(err)
	}
	return store.BudgetOverride{ProjectID: value.ProjectID, PerDayMaxUSD: result.PerDayMaxUSD, PerIssueMaxUSD: result.PerIssueMaxUSD, ExpiresAt: result.ExpiresAt, CreatedAt: result.CreatedAt, Reason: result.Reason}, nil
}

func (s *hubBudgetOverrideStore) ClearBudgetOverride(ctx context.Context, id string) error {
	native, err := s.project(id)
	if err != nil {
		return err
	}
	return budgetOverrideHubError(native.Transport().Request(ctx, http.MethodDelete, native.Scope()+"/budget-override", tracker.Mutation{IdempotencyKey: rand.Text()}, nil))
}

func markFlagRequired(cmd *cobra.Command, name string) {
	if err := cmd.MarkFlagRequired(name); err != nil {
		panic(err)
	}
}

func budgetConfig(projectID string, cfg workflowconfig.Budget, overrides budget.OverrideStore) budget.Config {
	return budget.Config{Enabled: cfg.Enabled, ProjectID: strings.TrimSpace(projectID), PerDayMaxUSD: cfg.PerDayMaxUSD, PerIssueMaxUSD: cfg.PerIssueMaxUSD, Overrides: overrides}
}

func budgetLimits(cfg workflowconfig.Budget) budget.OverrideLimits {
	return budget.OverrideLimits{MaxDuration: time.Duration(cfg.OverrideMaxDurationSeconds) * time.Second, MaxMultiplier: cfg.OverrideMaxMultiplier}
}

func writeBudgetOverride(out CommandOutput, override store.BudgetOverride, now time.Time) error {
	result := newBudgetOverrideResult(override, now)
	return out.Write(func(out io.Writer) error {
		_, err := fmt.Fprintf(out, "Notional USD budget override for %s: daily %s, issue %s, expires %s (%s), reason: %s\n", result.ProjectID, optionalBudgetUSD(result.PerDayMaxUSD), optionalBudgetUSD(result.PerIssueMaxUSD), result.ExpiresAt.Format(time.RFC3339), result.Remaining, result.Reason)
		return err
	}, result)
}

func newBudgetOverrideResult(override store.BudgetOverride, now time.Time) budgetOverrideResult {
	remaining := override.ExpiresAt.Sub(now).Round(time.Second)
	if remaining < 0 {
		remaining = 0
	}
	return budgetOverrideResult{ProjectID: override.ProjectID, PerDayMaxUSD: override.PerDayMaxUSD, PerIssueMaxUSD: override.PerIssueMaxUSD, ExpiresAt: override.ExpiresAt, CreatedAt: override.CreatedAt, Reason: override.Reason, Remaining: remaining.String()}
}

func optionalBudgetUSD(value *float64) string {
	if value == nil {
		return "base"
	}
	return budget.FormatUSD(*value)
}
