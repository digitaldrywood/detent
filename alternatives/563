package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
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
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, err
	}
	cfg, _, err := opts.readProject(resolution.Path, strings.TrimSpace(projectID))
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, err
	}
	if len(cfg.Projects) != 1 {
		return nil, workflowconfig.Workflow{}, func() {}, projectNotFoundError(projectID, cfg.Projects)
	}
	workflow, err := project.LoadWorkflowContext(ctx, cfg.Projects[0])
	if err != nil {
		return nil, workflowconfig.Workflow{}, func() {}, fmt.Errorf("load project workflow: %w", err)
	}
	writer, closeStore, err := openBudgetOverrideStoreAt(ctx, filepath.Join(filepath.Dir(resolution.Path), "detent.db"))
	return writer, workflow, closeStore, err
}

func openBudgetOverrideStore(ctx context.Context, configPath string, opts options) (budget.OverrideWriter, func(), error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return nil, func() {}, err
	}
	return openBudgetOverrideStoreAt(ctx, filepath.Join(filepath.Dir(resolution.Path), "detent.db"))
}

func openBudgetOverrideStoreAt(ctx context.Context, path string) (budget.OverrideWriter, func(), error) {
	backend, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
	if err != nil {
		return nil, func() {}, err
	}
	writer, ok := backend.(budget.OverrideWriter)
	if !ok {
		return nil, func() {}, errors.Join(errors.New("runtime store does not support budget overrides"), backend.Close())
	}
	return writer, func() {
		if err := backend.Close(); err != nil {
			slog.Warn("close budget override store", "error", err)
		}
	}, nil
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
