package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/serviceapi"
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
	for _, healthy := range []bool{true, false} {
		name := "no runnable project reports before exit"
		if healthy {
			name = "native stays online and github recovers"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			githubPath := writeWorkflowFile(t)
			raw, err := os.ReadFile(githubPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(githubPath, []byte(strings.Replace(string(raw), "kind: memory", "kind: github\n  repository: owner/site\n  github_status_source: label\n  status_label_prefix: 'detent:'", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			projects := []globalconfig.Project{{ID: "github", Workflow: githubPath, Workdir: filepath.Dir(githubPath), Weight: 1}}
			if healthy {
				path := writeWorkflowFile(t)
				nativeRaw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(nativeRaw), "kind: memory", "kind: hub_native", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				projects = append(projects, globalconfig.Project{ID: "native", Workflow: path, Workdir: filepath.Dir(path), Weight: 1})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var authenticated atomic.Bool
			tracker := memory.New(memory.Config{})
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
				})
			manager, err := project.NewManager(project.ManagerConfig{Projects: projects}, project.ManagerDependencies{ProjectFactory: factory, Logger: logger, RetrySleep: func(ctx context.Context, _ time.Duration) error {
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
			if (err == nil) != healthy || reported == healthy {
				t.Fatalf("startup error=%v, reported=%v, healthy=%v", err, reported, healthy)
			}
			problems := runnerStartupProblems(manager.Registry())
			if len(problems) != 1 || problems[0].ProjectID != "github" || !strings.Contains(problems[0].Message, "GITHUB_TOKEN is not set") || !strings.Contains(problems[0].FixHint, "gh auth login") || runnerauth.ValidateReportedProblems(problems) != nil {
				t.Fatalf("startup problems=%+v", problems)
			}
			if healthy {
				native, ok := manager.Registry().Get("native")
				if !ok || native.Workflow().Config.Tracker.Kind != "hub_native" || !native.Running() {
					t.Fatal("native project did not remain running")
				}
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
	return s.connector, id == "native"
}

func (*startupNativeScheduling) HeartbeatInterval() time.Duration { return time.Hour }

func (*startupNativeScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return nil, nil
}
