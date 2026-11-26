package runnerboot_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeadlessRunnerAppliesHubConfigurationCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	identityPath := filepath.Join(root, "private", "identity.json")
	desired := runnerauth.ProjectConfiguration{ProjectID: "prj_example", Authority: "hub_configuration", ConfigRevision: strings.Repeat("d", 64), Source: "cloud_workflow", EffectivePolicy: &policy.Descriptor{ID: "policy_example"}, ObservedAt: time.Now().UTC()}
	request := runnerauth.ProjectConfigurationRequest{RequestID: "detach-example", Operation: "detach_local_project", ProjectID: "prj_example", Desired: &desired}
	var mu sync.Mutex
	var snapshot runnerauth.RoutingSnapshot
	var reports []*runnerauth.ProjectConfiguration
	var identity runnerauth.Identity
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/capabilities"):
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeProviderCapacityCapability, tracker.NativeProjectConfigurationCapability, tracker.NativeHubProjectConfigurationCapability}})
		case strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID):
			_ = json.NewEncoder(w).Encode(identity)
		case strings.HasSuffix(r.URL.Path, "/projects/prj_example"):
			_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_example", OrganizationID: "org_example", Profile: "native"})
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var body struct {
				ProjectConfiguration *runnerauth.ProjectConfiguration `json:"project_configuration"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, body.ProjectConfiguration)
			response := snapshot
			if len(reports) == 1 && body.ProjectConfiguration != nil {
				response.ProjectConfiguration = &desired
				response.ProjectConfigurationRequest = &request
			}
			_ = json.NewEncoder(w).Encode(response)
		case strings.HasSuffix(r.URL.Path, "/update/receipt"):
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("null"))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	file, err := runnerauth.Initialize(identityPath, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_example"
	file.Identity.ProjectIDs = []tracker.ProjectID{"prj_example"}
	file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	file.Identity.Operations = []string{runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events}
	if err := runnerauth.Save(identityPath, file); err != nil {
		t.Fatal(err)
	}
	identity = file.Identity
	snapshot = runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1, ProjectIDs: file.Identity.ProjectIDs}.Normalized()}
	if err := runnerauth.SaveRoutingCache(identityPath, snapshot); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "global.yaml")
	cfg, err := globalconfig.DefaultAt(configPath, globalconfig.WithHome(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.MaxConcurrentAgents = 1
	cfg.Client.URL, cfg.Client.IdentityFile, cfg.Client.OrganizationID = server.URL, identityPath, "org_example"
	cfg.Client.Capacity, cfg.Client.HeartbeatIntervalSeconds = 1, 1
	if err := globalconfig.Write(configPath, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	command := cli.NewRootCommand(ctx, cli.WithVersion("dev"), cli.WithShutdownController(cli.NewShutdownController()))
	command.SetArgs([]string{"--config", configPath, "--headless"})
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- command.ExecuteContext(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	var stopped error
	var exited bool
	testenv.WaitUntil(t, ctx, func() error {
		mu.Lock()
		defer mu.Unlock()
		if len(reports) >= 2 || exited {
			return nil
		}
		select {
		case stopped, exited = <-done:
			exited = true
			return nil
		default:
			return errors.New("runner has not reported after the Hub configuration command")
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 {
		t.Fatalf("runner stopped before acknowledging the Hub configuration command: %v", stopped)
	}
	if reports[0] == nil {
		t.Fatal("headless runner heartbeat carried no project configuration report; the configuration owner is not installed")
	}
	report := reports[1]
	if report == nil || report.RequestID != request.RequestID || report.ProjectID != desired.ProjectID || report.Authority != "hub_configuration" || report.ConfigRevision != desired.ConfigRevision || report.Source != desired.Source || !report.Applied || !report.Saved || report.RuntimeRegistered || report.Constraint != "" {
		t.Fatalf("headless runner acknowledgment = %+v, want the runtime project manager to apply the delivered detach", report)
	}
}
