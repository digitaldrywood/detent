package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func resolveRunnerProjectGitHubToken(ctx context.Context, cfg globalconfig.Config, selected globalconfig.Project, workflow workflowconfig.Config, deps runtimeDeps) (string, error) {
	deps = deps.withDefaults()
	if !trackerUsesGitHubToken(workflow.Tracker.Kind) {
		return "", nil
	}
	if trackerHasGitHubAppCredentials(workflow.Tracker, deps.lookupEnv) {
		return "", nil
	}
	cfg.Projects = []globalconfig.Project{selected}
	deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
		return workflowconfig.Workflow{Config: workflow}, nil
	}
	cfg.Projects[0].WorkflowRef = ""
	token, _, err := resolveRuntimeGitHubToken(ctx, &cfg, deps)
	if err == nil && githubTokenSentinel(token.Value) {
		token, err = resolveConfiguredGitHubToken(ctx, token.Value, deps)
	}
	if errors.Is(err, ErrGitHubAuth) && strings.TrimSpace(cfg.GitHubToken) == "" {
		resolved, authErr := resolveConfiguredGitHubToken(ctx, "gh", deps)
		if authErr == nil {
			return resolved.Value, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return token.Value, err
}

func runnerStartupProblems(registry *project.Registry) []runnerauth.Problem {
	var problems []runnerauth.Problem
	for _, runtimeProject := range registry.List() {
		problems = append(problems, runtimeProject.RunnerProblems()...)
	}
	for _, health := range registry.Health() {
		if health.LastError == "" {
			continue
		}
		problem := runnerauth.NewProblem("settings_invalid")
		problem.ProjectID = health.Project.ID
		problem.Message = health.LastError
		if len(problem.Message) > 1000 {
			problem.Message = strings.ToValidUTF8(problem.Message[:1000], "")
		}
		if strings.Contains(health.LastError, "github_token") || strings.Contains(health.LastError, "tracker.api_key") {
			problem.FixHint = githubAuthHint
		}
		problems = append(problems, problem)
	}
	return problems
}

func startRunnerProjects(ctx context.Context, manager *project.Manager, heartbeat any) error {
	if err := manager.Start(ctx); err != nil {
		return err
	}
	var startupErr error
	health := manager.Registry().Health()
	for _, selected := range health {
		if selected.LastError == "" || selected.Transient && !selected.RetryStopped {
			return nil
		}
		startupErr = errors.Join(startupErr, fmt.Errorf("project %s: %s", selected.Project.ID, selected.LastError))
	}
	if len(health) == 0 {
		return nil
	}
	if reporter, ok := heartbeat.(runnerHeartbeatSource); ok {
		startupErr = errors.Join(startupErr, reporter.Heartbeat(ctx))
	}
	return WrapValidation(fmt.Errorf("no runnable projects: %w", startupErr))
}
