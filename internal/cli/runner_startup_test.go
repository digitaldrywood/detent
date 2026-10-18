package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
)

func TestRunnerProjectCredentials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, global, apiKey, token string
		authError                         bool
		wantError                         bool
		wantToken                         string
	}{
		{name: "native ignores broken global auth", kind: "hub_native", global: "gh", authError: true},
		{name: "github missing auth", kind: "github", authError: true, wantError: true},
		{name: "github invalid stored auth", kind: "github", global: "gh", authError: true, wantError: true},
		{name: "github signed in", kind: "github", token: "test-token", wantToken: "test-token"},
		{name: "github configured token", kind: "github", global: "configured", wantToken: "configured"},
		{name: "github project token", kind: "github", apiKey: "$PROJECT_TOKEN", wantToken: "project-token"},
		{name: "github project gh", kind: "github", apiKey: "gh", token: "test-token", wantToken: "test-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind, cfg.Tracker.APIKey = test.kind, test.apiKey
			calls := 0
			token, err := resolveRunnerProjectGitHubToken(t.Context(), globalconfig.Config{GitHubToken: test.global}, globalconfig.Project{ID: "site"}, cfg, runtimeDeps{
				lookupEnv: mapLookup(map[string]string{"PROJECT_TOKEN": "project-token"}),
				ghAuthToken: func(context.Context) (string, error) {
					calls++
					if test.authError {
						return "", errors.New("gh auth status failed: exit status 1")
					}
					return test.token, nil
				},
			})
			if (err != nil) != test.wantError || token != test.wantToken {
				t.Fatalf("resolved token matches=%v, error=%v", token == test.wantToken, err)
			}
			if test.kind == "hub_native" && calls != 0 {
				t.Fatal("native project probed GitHub authentication")
			}
		})
	}
}

func TestRunnerStartupProjectOutcomes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                             string
		healthy, policyPending, terminal bool
	}{
		{name: "no runnable project reports before exit", terminal: true},
		{name: "pending policy keeps host online until approval", policyPending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			githubPath := writeWorkflowFile(t)
			raw, err := os.ReadFile(githubPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(githubPath, []byte(strings.Replace(string(raw), "kind: hub_native", "kind: hub_native\n  repository: owner/site\n  github_status_source: label\n  status_label_prefix: 'detent:'\nworker:\n  github_token: gh", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			projects := []globalconfig.Project{{ID: "github", Workflow: githubPath, Workdir: filepath.Dir(githubPath), Weight: 1}}
			if test.healthy {
				path := writeWorkflowFile(t)
				nativeRaw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(nativeRaw), "kind: hub_native", "kind: hub_native", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				projects = append(projects, globalconfig.Project{ID: "native", Workflow: path, Workdir: filepath.Dir(path), Weight: 1})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var authenticated atomic.Bool
			tracker := fakehub.New(fakehub.Config{})
			scheduling := &startupNativeScheduling{connector: tracker}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			retry := make(chan struct{})
			retried := make(chan struct{}, 1)
			factory := withRunnerFactoryWithProjectTokens(ctx, project.Dependencies{Runner: &workflowStartupRunner{started: make(chan struct{}, 1)}, Connector: tracker, Scheduling: scheduling, Logger: logger}, nil, nil, serviceapi.Connection{}, nil, nil,
				func(ctx context.Context, selected globalconfig.Project, cfg workflowconfig.Config) (string, error) {
					token, err := resolveRunnerProjectGitHubToken(ctx, globalconfig.Config{}, selected, cfg, runtimeDeps{
						lookupEnv: func(string) string { return "" },
						ghAuthToken: func(context.Context) (string, error) {
							if authenticated.Load() {
								return "repaired-token", nil
							}
							return "", errors.New("gh auth status failed: exit status 1")
						},
					})
					if authenticated.Load() && selected.ID == "github" {
						defer func() { retried <- struct{}{} }()
					}
					return token, err
				}, nil)
			if test.policyPending || test.terminal {
				approvedFactory := factory
				factory = func(cfg globalconfig.Project) (*project.Project, error) {
					if !authenticated.Load() {
						if test.terminal {
							return nil, errors.Join(project.ErrProjectDefinition, errors.New("github_token: GITHUB_TOKEN is not set"))
						}
						return nil, errors.Join(project.ErrProjectDefinition, connector.NewRetryableError("policy_mismatch: repository policy approval pending"))
					}
					return approvedFactory(cfg)
				}
			}
			manager, err := projectfixture.NewManager(project.ManagerConfig{Projects: projects}, project.ManagerDependencies{ProjectFactory: factory, Logger: logger, RetrySleep: func(ctx context.Context, _ time.Duration) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retry:
					return nil
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				manager.Wait()
				for _, selected := range manager.Registry().List() {
					if err := selected.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			reported := false
			heartbeat := runnerHeartbeatFunc(func(context.Context) error {
				reported = len(runnerStartupProblems(manager.Registry())) == 1
				return nil
			})
			err = startRunnerProjects(ctx, manager, heartbeat)
			staysOnline := !test.terminal
			if (err == nil) != staysOnline || reported == staysOnline {
				t.Fatalf("startup error=%v, reported=%v, staysOnline=%v", err, reported, staysOnline)
			}
			problems := runnerStartupProblems(manager.Registry())
			if len(problems) != 1 || problems[0].ProjectID != "github" || runnerauth.ValidateReportedProblems(problems) != nil {
				t.Fatalf("startup problems=%+v", problems)
			}
			if test.policyPending {
				pending, ok := manager.Registry().Pending("github")
				if !ok || pending.RetryStopped || pending.NextRetryAt.IsZero() || manager.Registry().Len() != 0 || !strings.Contains(problems[0].Message, "policy_mismatch") {
					t.Fatalf("pending policy admitted work or lost retry: %+v", pending)
				}
				if err := heartbeat.Heartbeat(ctx); err != nil || !reported || ctx.Err() != nil {
					t.Fatalf("host heartbeat unavailable while awaiting policy: %v", err)
				}
			} else if !strings.Contains(problems[0].Message, "GITHUB_TOKEN is not set") || !strings.Contains(problems[0].FixHint, "gh auth login") {
				t.Fatalf("startup credential problem=%+v", problems[0])
			}
			if test.healthy {
				native, ok := manager.Registry().Get("native")
				if !ok || native.Workflow().Config.Tracker.Kind != "hub_native" || !native.Running() {
					t.Fatal("native project did not remain running")
				}
			}
			if staysOnline {
				authenticated.Store(true)
				close(retry)
				select {
				case <-retried:
				case <-time.After(5 * time.Second):
					t.Fatal("project credentials were not retried")
				}
				manager.Wait()
				if github, ok := manager.Registry().Get("github"); !ok || !github.Running() || len(runnerStartupProblems(manager.Registry())) != 0 {
					t.Fatal("repaired project did not start and clear its problem")
				}
			}
		})
	}
}

type startupNativeScheduling struct {
	orchestrator.SchedulingSource
	connector connector.Connector
}

func (s *startupNativeScheduling) ConnectorForProject(id string) (connector.Connector, bool) {
	return s.connector, id == "native" || id == "github"
}

func (*startupNativeScheduling) HeartbeatInterval() time.Duration { return time.Hour }

func (*startupNativeScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return nil, nil
}

func TestRunnerStartupProblemsSeparateProviderStartupFromSettings(t *testing.T) {
	t.Parallel()
	registry := project.NewRegistry()
	for id, message := range map[string]string{
		"provider": project.ProviderStartupFailed + ": scheduling source unavailable\nagent provider model catalog unavailable: provider models for backend codex: wait for initialize response: context deadline exceeded",
		"startup":  "project definition failed\n" + project.ProviderStartupFailed + ": scheduling source unavailable\nagent provider model catalog unavailable: provider models for backend codex: authentication required",
		"policy":   "repository policy reload rejected: policy_mismatch",
	} {
		if err := registry.SetPending(globalconfig.Project{ID: id}, project.RuntimeError{Message: message}); err != nil {
			t.Fatal(err)
		}
	}
	codes := map[string]string{}
	for _, problem := range runnerStartupProblems(registry) {
		codes[problem.ProjectID] = problem.Code
	}
	if want := map[string]string{"provider": "backend_probe_failed", "startup": "backend_probe_failed", "policy": "settings_invalid"}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("startup problem codes = %v, want %v", codes, want)
	}
	if err := runnerauth.ValidateReportedProblems(runnerStartupProblems(registry)); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerStartupProblemsIdentifyMissingPrerequisite(t *testing.T) {
	t.Parallel()
	registry := project.NewRegistry()
	message := project.ProviderStartupFailed + ": scheduling source unavailable: wait for initialize response: context deadline exceeded: Codex could not find bubblewrap on PATH"
	if err := registry.SetPending(globalconfig.Project{ID: "example-project"}, project.RuntimeError{Message: message}); err != nil {
		t.Fatal(err)
	}
	problems := runnerStartupProblems(registry)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v", problems)
	}
	problem := problems[0]
	if runtime.GOOS == "linux" {
		if problem.Code != "backend_missing" || !strings.Contains(problem.Message, "bubblewrap") || problem.FixCommand != "sudo apt-get install bubblewrap socat" || !strings.Contains(problem.ErrorOutput, "bubblewrap") {
			t.Fatalf("missing prerequisite was obscured: %+v", problem)
		}
	} else if problem.Code != "backend_probe_failed" {
		t.Fatalf("Linux prerequisite diagnosis applied to another platform: %+v", problem)
	}
}

func TestRunnerProviderTimeoutsRecoverWithBackoff(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := writeWorkflowFile(t)
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "native", Workflow: path, Workdir: filepath.Dir(path), Weight: 1}}}
	var calls atomic.Int32
	source := runnerProviderReports(ctx, func() globalconfig.Config { return cfg }, "example-runner", time.Now, func(context.Context, workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
		if calls.Add(1) <= 3 {
			return nil, context.DeadlineExceeded
		}
		return []runner.AgentModel{{ID: "example-model"}}, nil
	}, nil)
	tracker := fakehub.New(fakehub.Config{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheduling := &catalogStartupScheduling{startupNativeScheduling: &startupNativeScheduling{connector: tracker}, source: source}
	factory := withRunnerFactoryWithProjectTokens(ctx, project.Dependencies{Runner: &workflowStartupRunner{started: make(chan struct{}, 1)}, Connector: tracker, Scheduling: scheduling, Logger: logger}, nil, nil, serviceapi.Connection{}, nil, nil, func(context.Context, globalconfig.Project, workflowconfig.Config) (string, error) { return "", nil }, nil)
	delays := make(chan time.Duration)
	retry := make(chan struct{})
	manager, err := projectfixture.NewManager(project.ManagerConfig{Projects: cfg.Projects}, project.ManagerDependencies{ProjectFactory: factory, Logger: logger, RetryJitter: func(time.Duration) time.Duration { return 0 }, RetrySleep: func(ctx context.Context, delay time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delays <- delay:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry:
			return nil
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		manager.Wait()
		for _, selected := range manager.Registry().List() {
			if err := selected.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := startRunnerProjects(ctx, manager, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		select {
		case got := <-delays:
			if got != want {
				t.Fatalf("retry delay = %s, want %s", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if problems := runnerStartupProblems(manager.Registry()); len(problems) != 1 || problems[0].Code != "backend_probe_failed" {
			t.Fatalf("timeout lost its recoverable startup problem: %+v", problems)
		}
		retry <- struct{}{}
	}
	manager.Wait()
	selected, ok := manager.Registry().Get("native")
	if !ok || !selected.Running() || len(runnerStartupProblems(manager.Registry())) != 0 {
		t.Fatal("runner did not resume scheduling after repeated provider timeouts")
	}
	reports, err := source()
	if err != nil || len(reports) != 1 || !reports[0].Supports(providercapacity.Requirement{Backend: "codex", Model: "example-model"}) {
		t.Fatalf("recovered runner did not advertise its provider: %+v %v", reports, err)
	}
}
