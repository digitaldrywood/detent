package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/codex"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestRunnerProviderCatalogStartupRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := writeWorkflowFile(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	workflow := strings.Replace(string(raw), "codex:\n", "codex:\n  read_timeout_ms: 60000\n", 1)
	if err := os.WriteFile(path, []byte(workflow), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "native", Workflow: path, Workdir: filepath.Dir(path), Weight: 1}}}
	var healthy atomic.Bool
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	source := runnerProviderReports(ctx, func() globalconfig.Config { return cfg }, "example-runner", time.Now, func(ctx context.Context, backend workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
		if backend.CodexOptions().ReadTimeoutMS != 60000 {
			t.Errorf("effective read timeout = %d", backend.CodexOptions().ReadTimeoutMS)
		}
		factory := &catalogLifecycleFactory{test: t, healthy: healthy.Load()}
		server, err := codex.NewAppServer(factory, codex.WithReadTimeout(durationFromMillis(backend.CodexOptions().ReadTimeoutMS)), codex.WithLogger(logger))
		if err != nil {
			return nil, err
		}
		agent, err := codex.NewAgentBackend(server, codex.Options{})
		if err != nil {
			return nil, err
		}
		return agent.ListModels(ctx, runner.AgentProcessRequest{})
	}, nil)
	tracker := fakehub.New(fakehub.Config{})
	scheduling := &catalogStartupScheduling{startupNativeScheduling: &startupNativeScheduling{connector: tracker}, source: source}
	factory := withRunnerFactoryWithProjectTokens(ctx, project.Dependencies{Runner: &workflowStartupRunner{started: make(chan struct{}, 1)}, Connector: tracker, Scheduling: scheduling, Logger: logger}, nil, nil, serviceapi.Connection{}, nil, nil, func(context.Context, globalconfig.Project, workflowconfig.Config) (string, error) { return "", nil }, nil)
	retry := make(chan struct{})
	manager, err := project.NewManager(project.ManagerConfig{Projects: cfg.Projects}, project.ManagerDependencies{ProjectFactory: factory, Logger: logger, RetrySleep: func(ctx context.Context, _ time.Duration) error {
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
	if err := startRunnerProjects(ctx, manager, runnerHeartbeatFunc(func(context.Context) error { return nil })); err != nil {
		t.Fatal(err)
	}
	pending, ok := manager.Registry().Pending("native")
	if !ok || pending.RetryStopped || manager.Registry().Len() != 0 {
		t.Fatalf("catalog failure did not leave recoverable registration: %+v", pending)
	}
	if problems := runnerStartupProblems(manager.Registry()); len(problems) != 1 || problems[0].Code != "backend_probe_failed" {
		t.Fatalf("catalog startup failure was not reported as a backend probe failure: %+v", problems)
	}
	reports, err := source()
	if err == nil || len(reports) != 0 || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("failed catalog became provider report: %+v %v", reports, err)
	}
	healthy.Store(true)
	reports, err = source()
	if err != nil || len(reports) != 1 || !reports[0].Supports(providercapacity.Requirement{Backend: "codex", Model: "example-model"}) || reports[0].Supports(providercapacity.Requirement{Backend: "codex", Model: "invented-model"}) || reports[0].Availability != "unknown" {
		t.Fatalf("recovered provider reports = %+v %v", reports, err)
	}
	retry <- struct{}{}
	finished := make(chan struct{})
	go func() { manager.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("project registration did not recover")
	}
	selected, ok := manager.Registry().Get("native")
	if !ok || !selected.Running() || len(runnerStartupProblems(manager.Registry())) != 0 {
		t.Fatal("catalog recovery did not register project and clear startup problem")
	}
}

type catalogStartupScheduling struct {
	*startupNativeScheduling
	source func() ([]providercapacity.Report, error)
}

func (s *catalogStartupScheduling) PrepareProject(context.Context, string) error {
	_, err := s.source()
	if err != nil {
		return errors.Join(orchestrator.ErrSchedulingUnavailable, err)
	}
	return nil
}

type catalogLifecycleFactory struct {
	test    *testing.T
	healthy bool
}

func (f *catalogLifecycleFactory) NewTransport(ctx context.Context) (codex.Transport, error) {
	var cmd *exec.Cmd
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunnerProviderCatalogHelper$")
		cmd.Env = append(os.Environ(), "DETENT_PROVIDER_CATALOG_HELPER=1")
		if f.healthy {
			cmd.Env = append(cmd.Env, "DETENT_PROVIDER_CATALOG_HEALTHY=1")
		}
		return cmd
	})
	if err != nil {
		return nil, err
	}
	transport, err := factory.NewTransport(ctx)
	if err != nil {
		return nil, err
	}
	return &catalogLifecycleTransport{Transport: transport, test: f.test, cmd: cmd}, nil
}

type catalogLifecycleTransport struct {
	codex.Transport
	test *testing.T
	cmd  *exec.Cmd
}

func (t *catalogLifecycleTransport) Close(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 59*time.Second {
		t.test.Error("project catalog cleanup lost configured timeout")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := t.Transport.Close(expired)
	evidence := t.Transport.(interface {
		StartupProcessEvidence() backendcapacity.StartupProcessEvidence
	}).StartupProcessEvidence()
	if !evidence.ExitObserved || !evidence.CleanupComplete || !evidence.TerminationRequested || t.cmd.ProcessState == nil {
		t.test.Errorf("catalog helper not reaped: %+v", evidence)
	}
	return err
}

func TestRunnerProviderCatalogHelper(t *testing.T) {
	if os.Getenv("DETENT_PROVIDER_CATALOG_HELPER") != "1" {
		return
	}
	codec := codex.NewCodec(os.Stdin, os.Stdout)
	for {
		msg, err := codec.ReadMessage()
		if err != nil {
			os.Exit(2)
		}
		response := codex.Message{ID: msg.ID}
		switch msg.Method {
		case "initialize":
			response.Result = json.RawMessage(`{"userAgent":"fixture"}`)
		case "initialized":
			continue
		case "model/list":
			if os.Getenv("DETENT_PROVIDER_CATALOG_HEALTHY") == "1" {
				response.Result = json.RawMessage(`{"data":[{"id":"example-model","model":"example-model"}]}`)
			} else {
				response.Error = &codex.RPCError{Code: -32000, Message: "authentication required"}
			}
		default:
			os.Exit(3)
		}
		if err := codec.WriteMessage(response); err != nil {
			os.Exit(4)
		}
		if msg.Method == "model/list" {
			time.Sleep(time.Hour)
			os.Exit(5)
		}
	}
}
