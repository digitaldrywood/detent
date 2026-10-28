package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerCheckoutRepositoryReportsOnlyCanonicalOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name, remote, want string
		externalWorkflow   bool
		missingWorkflow    bool
		invalidWorkflow    bool
		missingGit         bool
		refWorkflow        bool
		wantReady          bool
	}{
		{name: "registration default workflow", remote: "git@github.com:Acme/Private.git", want: "Acme/Private", wantReady: true},
		{name: "private HTTPS origin", remote: "https://alice:private-secret@github.com/Acme/Private.git", want: "Acme/Private", wantReady: true},
		{name: "unsupported origin", remote: "https://alice:private-secret@example.test/Acme/Private.git", wantReady: true},
		{name: "GitLab origin with external workflow", remote: "git@gitlab.com:Acme/Private.git", externalWorkflow: true, wantReady: true},
		{name: "local origin", remote: "../source.git", wantReady: true},
		{name: "configured workflow outside checkout", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, want: "Acme/Private", wantReady: true},
		{name: "missing configured workflow despite default", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, missingWorkflow: true, want: "Acme/Private"},
		{name: "invalid configured workflow despite default", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, invalidWorkflow: true, want: "Acme/Private"},
		{name: "missing default workflow", remote: "git@github.com:Acme/Private.git", missingWorkflow: true, want: "Acme/Private"},
		{name: "missing Git origin", externalWorkflow: true, wantReady: true},
		{name: "missing Git checkout", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, missingGit: true},
		{name: "configured Git ref workflow absent locally", remote: "git@github.com:Acme/Private.git", refWorkflow: true, want: "Acme/Private", wantReady: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workdir := filepath.Join(root, "checkout")
			checkout(t, workdir)
			runDoctorWorkflowSourceGit(t, workdir, "remote", "remove", "origin")
			if test.remote != "" {
				runDoctorWorkflowSourceGit(t, workdir, "remote", "add", "origin", test.remote)
			}
			selected := globalconfig.Project{Workdir: workdir}
			if test.externalWorkflow {
				selected.Workflow = filepath.Join(root, "WORKFLOW.md")
				if test.invalidWorkflow {
					if err := os.WriteFile(selected.Workflow, []byte("---\ntracker: [\n---\nPRIVATE WORKFLOW\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if !test.missingWorkflow {
					if err := os.WriteFile(selected.Workflow, []byte(mustRead(t, filepath.Join(workdir, "WORKFLOW.md"))), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(workdir, "WORKFLOW.md")); err != nil {
						t.Fatal(err)
					}
					runDoctorWorkflowSourceGit(t, workdir, "add", "-u")
					runDoctorWorkflowSourceGit(t, workdir, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "externalize workflow")
				}
			} else if test.missingWorkflow || test.refWorkflow {
				if err := os.Remove(filepath.Join(workdir, "WORKFLOW.md")); err != nil {
					t.Fatal(err)
				}
			}
			if test.refWorkflow {
				selected.Workflow, selected.WorkflowRef = "WORKFLOW.md", "HEAD"
			}
			if test.missingGit {
				if err := os.RemoveAll(filepath.Join(workdir, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if got := runnerCheckoutRepository(t.Context(), selected); got != test.want || strings.Contains(got, "private-secret") {
				t.Fatalf("repository = %q, want %q", got, test.want)
			}
			if got := runnerCheckoutReady(t.Context(), selected); got != test.wantReady {
				t.Fatalf("checkout ready = %v, want %v", got, test.wantReady)
			}
		})
	}
}

func TestRunnerHubTarget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		url          string
		organization string
		wantBase     string
		wantOrg      tracker.OrganizationID
		wantErr      bool
	}{
		{name: "shared entry URL", url: "https://cloud.detent.build/organizations/org_abc/", wantBase: "https://cloud.detent.build/organizations/org_abc", wantOrg: "org_abc"},
		{name: "legacy hosted URL remains stable", url: "https://hub.detent.build/organizations/org_abc", wantBase: "https://hub.detent.build/organizations/org_abc", wantOrg: "org_abc"},
		{name: "staging Cloud URL", url: "https://staging.cloud.detent.build/organizations/org_abc", wantBase: "https://staging.cloud.detent.build/organizations/org_abc", wantOrg: "org_abc"},
		{name: "self-hosted with flag", url: "https://hub.example.test", organization: "org_self", wantBase: "https://hub.example.test", wantOrg: "org_self"},
		{name: "flag agrees with URL", url: "https://cloud.detent.build/organizations/org_abc", organization: "org_abc", wantBase: "https://cloud.detent.build/organizations/org_abc", wantOrg: "org_abc"},
		{name: "flag disagrees with URL", url: "https://cloud.detent.build/organizations/org_abc", organization: "org_other", wantErr: true},
		{name: "no organization", url: "https://hub.example.test", wantErr: true},
		{name: "not an organization ID", url: "https://hub.example.test/organizations/acme", wantErr: true},
		{name: "empty", url: " ", wantErr: true},
		{name: "query string", url: "https://cloud.detent.build/organizations/org_abc?x=1", wantErr: true},
		{name: "no host", url: "/organizations/org_abc", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base, org, err := runnerHubTarget(test.url, test.organization)
			if test.wantErr {
				if err == nil {
					t.Fatalf("runnerHubTarget(%q, %q) = %q, %q; want an error", test.url, test.organization, base, org)
				}
				return
			}
			if err != nil || base != test.wantBase || org != test.wantOrg {
				t.Fatalf("runnerHubTarget(%q, %q) = %q, %q, %v; want %q, %q", test.url, test.organization, base, org, err, test.wantBase, test.wantOrg)
			}
		})
	}
}

func TestHubRunnerRegisterRequiresServiceManagerBeforeEnrollment(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"freebsd", "plan9"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, nil)
			root := t.TempDir()
			cmd := newHubRunnerRegisterCommand("test", func(string) string { return "" }, runnerServiceStarterForOS(options{}, goos))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--service", "--url", hub.server.URL + "/organizations/org_example", "--token", "det_enroll_example", "--config", filepath.Join(root, "global.yaml"), "--workspace-root", filepath.Join(root, "work")})
			if err := cmd.Execute(); !errors.Is(err, servicepkg.ErrUnsupported) {
				t.Fatalf("register error = %v, want unsupported service", err)
			}
			if got := hub.redeemed.Load(); got != 0 {
				t.Fatalf("redeemed %d tokens without a service manager", got)
			}
		})
	}
}

func TestRunnerStartCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ goos, path, want string }{
		{"windows", `C:\Users\Example's $user\runner\global.yaml`, `detent --config 'C:\Users\Example''s $user\runner\global.yaml' --headless`},
		{"windows", `C:\runner\global.yaml`, `detent --config 'C:\runner\global.yaml' --headless`},
		{"windows", `C:\x‘; Start-Process calc; #\global.yaml`, `detent --config 'C:\x‘‘; Start-Process calc; #\global.yaml' --headless`},
		{"windows", `C:\x’; Start-Process calc; #\global.yaml`, `detent --config 'C:\x’’; Start-Process calc; #\global.yaml' --headless`},
		{"windows", `C:\x‚; Start-Process calc; #\global.yaml`, `detent --config 'C:\x‚‚; Start-Process calc; #\global.yaml' --headless`},
		{"windows", `C:\x‛; Start-Process calc; #\global.yaml`, `detent --config 'C:\x‛‛; Start-Process calc; #\global.yaml' --headless`},
		{"darwin", "/Users/example's folder/global.yaml", `detent --config '/Users/example'\''s folder/global.yaml' --headless`},
		{"linux", "/home/example/global.yaml", "detent --config /home/example/global.yaml --headless"},
	} {
		t.Run(test.goos+test.path, func(t *testing.T) {
			t.Parallel()
			if got := runnerStartCommand(test.path, test.goos); got != test.want {
				t.Fatalf("start command = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunnerServiceSupportedPlatforms(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"windows", "darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			if runnerServiceStarterForOS(options{}, goos) == nil {
				t.Fatal("supported platform has no service starter")
			}
		})
	}
}

func TestRunnerConfigGitHubToken(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		existing *string
	}{
		{name: "new config"},
		{name: "existing custom credential", existing: new("$CUSTOM_GITHUB_TOKEN")},
		{name: "existing omitted credential", existing: new("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "identity.json"), workspaces: filepath.Join(root, "work")}
			config := runnerConfig("https://hub.example.test", "org_example", "Build host", 2, paths)
			var before string
			if test.existing != nil {
				before = "github_token: " + *test.existing + "\n"
				if err := os.WriteFile(paths.config, []byte(before), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			created, err := writeRunnerConfig(paths.config, config)
			if err != nil || created != (test.existing == nil) {
				t.Fatalf("created = %v, error = %v", created, err)
			}
			body := mustRead(t, paths.config)
			if test.existing != nil {
				if body != before {
					t.Fatal("existing credential setting was rewritten")
				}
				return
			}
			cfg, err := readRunnerSetupConfig(paths.config)
			if err != nil || cfg.GitHubToken != "gh" {
				t.Fatalf("generated GitHub credential source = %q, error = %v", cfg.GitHubToken, err)
			}
		})
	}
}

func TestRunnerGitHubAuthNextSteps(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		definition string
		missing    bool
		authErr    error
		wantCheck  bool
		wantSteps  []string
	}{
		{name: "native PR landing signed in", definition: "tracker:\n  kind: hub_native\ndeliverable:\n  github_pull_request: true\n", wantCheck: true},
		{name: "native PR landing signed out", definition: "tracker:\n  kind: hub_native\ndeliverable:\n  github_pull_request: true\n", authErr: errors.New("signed out"), wantCheck: true, wantSteps: []string{"Project affected needs GitHub authentication: run gh auth login"}},
		{name: "native Git landing", definition: "tracker:\n  kind: hub_native\n"},
		{name: "checkout not cloned", missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workflow := filepath.Join(root, "WORKFLOW.md")
			if !test.missing {
				if err := os.WriteFile(workflow, []byte("Work the issue.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte("schema: 1\n"+test.definition), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "affected", Workflow: workflow, Workdir: root}, {ID: "uncloned", Workflow: filepath.Join(root, "uncloned", "WORKFLOW.md")}}}
			if !test.missing {
				if _, err := project.LoadWorkflowContext(t.Context(), cfg.Projects[0]); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			steps, err := runnerGitHubAuthNextSteps(t.Context(), cfg, func(context.Context) error {
				checks++
				return test.authErr
			})
			wantChecks := 0
			if test.wantCheck {
				wantChecks = 1
			}
			if err != nil || checks != wantChecks || !slices.Equal(steps, test.wantSteps) {
				t.Fatalf("next steps = %v, error = %v, checks = %d; want %v, %d", steps, err, checks, test.wantSteps, wantChecks)
			}
		})
	}
}

type registerHub struct {
	setupProgress    atomic.Value
	localChecks      sync.Map
	server           *httptest.Server
	credential       atomic.Value
	identity         atomic.Value
	heartbeat        atomic.Value
	tier             atomic.Value
	redeemed         atomic.Int32
	requests         atomic.Int32
	projects         map[tracker.ProjectID]string
	cloneURLs        map[tracker.ProjectID]string
	features         []string
	repositories     map[tracker.ProjectID]string
	minimumVersion   string
	failCapabilities atomic.Bool
}

func newRegisterHub(t *testing.T, projects map[tracker.ProjectID]string) *registerHub {
	t.Helper()
	hub := &registerHub{projects: projects, repositories: make(map[tracker.ProjectID]string, len(projects))}
	for id := range projects {
		hub.repositories[id] = "acme/" + string(id)
	}
	hub.credential.Store("")
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/organizations/org_example/api/v2/organizations/org_example/") && !strings.HasPrefix(r.URL.Path, "/api/v2/organizations/org_example/") && !strings.HasSuffix(r.URL.Path, "/capabilities") {
			t.Errorf("request outside the organization: %s", r.URL.Path)
		}
		ids := make([]tracker.ProjectID, 0, len(projects))
		for id := range projects {
			ids = append(ids, id)
		}
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			if !strings.HasPrefix(r.URL.Path, "/organizations/org_example/") && r.Header.Get("X-Detent-Organization") != "org_example" {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "organization_required"})
				return
			}
			if hub.failCapabilities.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": hub.features, "minimum_runner_version": hub.minimumVersion})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runner-enrollments/redeem") {
			if r.Header.Get("Authorization") != "Bearer det_enroll_example" || hub.redeemed.Load() != 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var request runnerauth.Redemption
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.DisplayName != "Build host" || request.Capacity != 2 || !request.Valid() {
				t.Errorf("redemption = %+v", request)
			}
			identity := runnerauth.Identity{Binding: request.Binding, OrganizationID: "org_example", ProjectIDs: ids, Operations: []string{runnerauth.Read}, ExpiresAt: time.Now().Add(runnerauth.CredentialTTL)}
			hub.credential.Store(request.Credential)
			hub.identity.Store(identity)
			tier := request.IsolationTier
			if tier == "" {
				tier = "sandbox"
			}
			hub.tier.Store(tier)
			hub.redeemed.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(identity)
			return
		}
		credential, _ := hub.credential.Load().(string)
		if credential == "" || r.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized"})
			return
		}
		if identity, ok := hub.identity.Load().(runnerauth.Identity); ok && strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID) {
			_ = json.NewEncoder(w).Encode(identity)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/onboarding") {
			progress, _ := hub.setupProgress.Load().(onboarding.Progress)
			_ = json.NewEncoder(w).Encode(map[string]any{"progress": progress})
			return
		}
		if strings.Contains(r.URL.Path, "/machines/") && strings.HasSuffix(r.URL.Path, "/heartbeat") {
			var body struct {
				LocalChecks runnerauth.LocalChecks `json:"local_checks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if err := body.LocalChecks.Validate(); err != nil {
				t.Error(err)
			}
			for id := range projects {
				if strings.Contains(r.URL.Path, "/projects/"+string(id)+"/") {
					hub.localChecks.Store(id, body.LocalChecks)
				}
			}
			identity := hub.identity.Load().(runnerauth.Identity)
			_ = json.NewEncoder(w).Encode(runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Build host", State: "active", CapacityLimit: 2, ProjectIDs: ids}.Normalized()})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/policy/observed") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if identity, ok := hub.identity.Load().(runnerauth.Identity); ok && strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID+"/routing") {
			tier, _ := hub.tier.Load().(string)
			runner := runnerauth.Runner{Binding: identity.Binding, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Build host", State: "active", CapacityLimit: 2, ProjectIDs: ids, IsolationTier: tier}.Normalized()}
			if heartbeat, ok := hub.heartbeat.Load().(time.Time); ok {
				runner.LastHeartbeatAt = heartbeat
				runner.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{Provider: "openai"}}}
			}
			_ = json.NewEncoder(w).Encode(runner)
			return
		}
		for id, name := range projects {
			if strings.HasSuffix(r.URL.Path, "/projects/"+string(id)) {
				_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: id, Profile: "native", OrganizationID: "org_example", Name: name, CloneURL: hub.cloneURLs[id], Repository: hub.repositories[id], States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/projects/"+string(id)+"/work-items") {
				_ = json.NewEncoder(w).Encode(tracker.Page[tracker.NativeIssue]{Items: []tracker.NativeIssue{{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID("wi_" + string(id)), ProjectID: id, OrganizationID: "org_example", Number: 7}, State: "Todo"}}})
				return
			}
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(hub.server.Close)
	return hub
}

func runRegister(t *testing.T, env map[string]string, starter runnerServiceStarter, args ...string) (string, error) {
	t.Helper()
	return executeRegister(t, newHubRunnerRegisterCommandWithServiceName("test", "linux", func(name string) string { return env[name] }, starter, runnerPrivateLocation, func(context.Context, globalconfig.Config, string) error { return nil }, func(context.Context) error { return nil }, testRunnerServiceName), args...)
}

// Registration fixtures stay under t.TempDir even when worker TMPDIR is inside this repository.
func runRegisterInTestWorkspace(t *testing.T, env map[string]string, starter runnerServiceStarter, args ...string) (string, error) {
	t.Helper()
	return executeRegister(t, newHubRunnerRegisterCommandWithServiceName("test", "linux", func(name string) string { return env[name] }, starter, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil }, func(context.Context) error { return nil }, testRunnerServiceName), args...)
}

func executeRegister(t *testing.T, command *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())
	return output.String(), err
}

func TestHubRunnerRegisterServiceRuntime(t *testing.T) {
	t.Parallel()
	serviceErr := errors.New("service manager unavailable")
	probeErr := errors.New("commit policy unavailable")
	for _, test := range []struct {
		name        string
		configPort  int
		flags       []string
		wantPort    string
		wantArgs    []string
		factoryErr  error
		startErr    error
		probeErr    error
		wantFailure bool
	}{
		{name: "commit policy failure", probeErr: probeErr},
		{name: "no port flag", wantPort: "0"},
		{name: "configured port", configPort: 4100, wantPort: "4100"},
		{name: "explicit ephemeral port", flags: []string{"--port", "0"}, wantPort: "0", wantArgs: []string{"--port", "0"}},
		{name: "explicit port", flags: []string{"--port", "4200"}, wantPort: "4200", wantArgs: []string{"--port", "4200"}},
		{name: "negative port", flags: []string{"--port", "-1"}, wantFailure: true},
		{name: "service configuration failure", factoryErr: serviceErr, wantFailure: true},
		{name: "service start failure", startErr: serviceErr, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, nil)
			root := t.TempDir()
			configPath := filepath.Join(root, "runner config", "global.yaml")
			var captured servicepkg.Config
			runner := &serviceRunnerStub{startErrs: []error{test.startErr}, startResults: []servicepkg.StartResult{{Action: servicepkg.ActionInstalled, State: servicepkg.StateRunning}}, onStart: func() { hub.heartbeat.Store(time.Now()) }}
			var opts options
			command := NewRootCommand(t.Context(), func(o *options) {
				o.lookupEnv = func(string) string { return "" }
				WithServiceFactory(func(cfg servicepkg.Config) (ServiceRunner, error) {
					captured = cfg
					return runner, test.factoryErr
				})(o)
				opts = *o
			})
			runnerCommand, _, err := command.Find([]string{"hub", "runner"})
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range runnerCommand.Commands() {
				if child.Name() == "register" {
					runnerCommand.RemoveCommand(child)
				}
			}
			probes := 0
			probe := func(ctx context.Context, source string, fallback workspace.CommitIdentity) (workspace.CommitReport, error) {
				probes++
				identity := hub.identity.Load().(runnerauth.Identity)
				if ctx.Err() != nil || source != "" || fallback.Name != "Detent runner" || fallback.Email != string(identity.MachineID)+"@runners.detent.invalid" {
					t.Fatal("commit probe lost its context or enrollment identity")
				}
				return workspace.CommitReport{Identity: fallback, Signing: "ssh"}, test.probeErr
			}
			register := newHubRunnerRegisterCommandWithCommitReport("test", "linux", opts.lookupEnv, runnerServiceStarterFor(opts), func(string) error { return nil }, func(_ context.Context, cfg globalconfig.Config, _ string) error {
				cfg.Port = &test.configPort
				return globalconfig.Write(configPath, cfg)
			}, func(context.Context) error { return nil }, testRunnerServiceName, probe)
			runnerCommand.AddCommand(register)
			args := []string{"hub", "runner", "register", "--url", hub.server.URL + "/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", filepath.Join(root, "work"), "--service"}
			output, err := executeRegister(t, command, append(args, test.flags...)...)
			if probes != 1 || hub.redeemed.Load() != 1 {
				t.Fatalf("commit probes=%d token redemptions=%d", probes, hub.redeemed.Load())
			}
			if test.probeErr != nil {
				if !errors.Is(err, test.probeErr) || len(runner.startOptions) != 0 {
					t.Fatalf("commit policy failure did not stop service startup: %v", err)
				}
				if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("commit policy failure wrote a runner config: %v", err)
				}
				return
			}
			if test.wantFailure {
				if err == nil {
					t.Fatal("service failure returned success")
				}
				if !strings.Contains(ErrorDetail(err), "enrollment succeeded") || !strings.Contains(ErrorHint(err), "detent --config '"+configPath+"' --headless") {
					t.Fatalf("service failure lacks enrollment recovery: %v", err)
				}
				if test.factoryErr != nil && !errors.Is(err, test.factoryErr) || test.startErr != nil && !errors.Is(err, test.startErr) {
					t.Fatalf("service error was not preserved: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("register: %v\n%s", err, output)
				}
				if captured.Name != runnerServiceName || captured.ConfigPath != configPath || captured.DashboardURL != "http://localhost:"+test.wantPort {
					t.Fatalf("service config = %+v", captured)
				}
				want := append([]string{"--config", configPath}, test.wantArgs...)
				want = append(want, "--headless")
				if !reflect.DeepEqual(captured.Arguments, want) || len(runner.startOptions) != 1 || !runner.startOptions[0].Install {
					t.Fatalf("service arguments = %v, start options = %+v", captured.Arguments, runner.startOptions)
				}
				var registration runnerRegistration
				if err := json.Unmarshal([]byte(output), &registration); err != nil || !registration.ServiceRun || registration.Service != runnerServiceName {
					t.Fatalf("service registration = %+v, %v\n%s", registration, err, output)
				}
				identity := hub.identity.Load().(runnerauth.Identity)
				if registration.Commit.Identity.Name != "Detent runner" || registration.Commit.Identity.Email != string(identity.MachineID)+"@runners.detent.invalid" || registration.Commit.Signing != "ssh" {
					t.Fatalf("registration lost commit policy: %+v", registration.Commit)
				}
			}
		})
	}
}

func TestHubRunnerRegisterWritesAWorkingRunnerConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_site": "detent.build", "prj_ops": "Ops Tools"})
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "detent-runner", "global.yaml")
	workspaces := filepath.Join(root, "work")
	checkout(t, filepath.Join(workspaces, "detent.build"))
	var started []string
	starter := func(_ *cobra.Command, path string) error {
		started = append(started, path)
		return nil
	}
	args := []string{"--url", hub.server.URL + "/organizations/org_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaces, "--service"}
	output, err := runRegisterInTestWorkspace(t, map[string]string{"DETENT_RUNNER_ENROLLMENT_TOKEN": "det_enroll_example"}, starter, args...)
	if err != nil {
		t.Fatalf("register: %v\n%s", err, output)
	}
	if strings.Contains(output, "det_enroll_example") {
		t.Fatal("register output echoed the enrollment token")
	}
	var registration runnerRegistration
	if err := json.Unmarshal([]byte(output), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.Commit.Identity.Name == "" || registration.Commit.Identity.Email == "" || registration.Commit.Signing == "" {
		t.Fatalf("registration omitted commit policy: %+v", registration.Commit)
	}
	steps := strings.Join(registration.NextSteps, "\n")
	if len(started) != 1 || !registration.ServiceRun || steps != "" {
		t.Fatalf("service did not start with a missing checkout (%v):\n%s", started, output)
	}

	var written runnerConfigFile
	if err := yaml.Unmarshal([]byte(mustRead(t, configPath)), &written); err != nil || written.WorkspaceRoot != workspaces {
		t.Fatalf("written config = %+v, %v", written, err)
	}
	info, err := os.Stat(configPath)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v", info, err)
	}

	checkout(t, filepath.Join(workspaces, "ops-tools"))
	if err := os.WriteFile(configPath, []byte("# edited by the operator\n"+mustRead(t, configPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = runRegisterInTestWorkspace(t, map[string]string{}, starter, append(args, "--token", "det_enroll_example")...)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, output)
	}
	if hub.redeemed.Load() != 1 {
		t.Fatalf("rerun redeemed the token again: %d redemptions", hub.redeemed.Load())
	}
	var rerun runnerRegistration
	if err := json.Unmarshal([]byte(output), &rerun); err != nil {
		t.Fatalf("rerun output is not JSON: %v\n%s", err, output)
	}
	if len(started) != 2 || started[0] != configPath || rerun.Created || !rerun.ServiceRun || !strings.HasPrefix(mustRead(t, configPath), "# edited by the operator") {
		t.Fatalf("rerun: started %v, output:\n%s", started, output)
	}

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	identityPath := filepath.Join(filepath.Dir(configPath), "identity.json")
	file, err := runnerauth.Load(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceName != runnerServiceName || cfg.Client.URL != hub.server.URL+"/organizations/org_example" || cfg.Client.IdentityFile != identityPath || cfg.Client.OrganizationID != "org_example" || cfg.Client.Capacity != 2 || cfg.Client.DisplayName != "Build host" {
		t.Fatalf("client config = %+v, service %q", cfg.Client, cfg.ServiceName)
	}
	if len(cfg.Client.NativeProjects) != 0 || len(cfg.Projects) != 0 || cfg.WorkspaceRoot != workspaces {
		t.Fatalf("machine config = %+v", cfg)
	}
	if file.Identity.OrganizationID != "org_example" || !strings.Contains(output, file.Identity.RunnerID) {
		t.Fatalf("identity = %+v", file.Identity)
	}
}

func TestHubRunnerRegisterAccessConfiguration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, goos, selected, want string
		refused                    bool
	}{
		{"linux default", "linux", "", "sandbox", false},
		{"linux sandbox", "linux", "sandbox", "sandbox", false},
		{"linux full access", "linux", "native-trusted", "native-trusted", false},
		{"darwin default", "darwin", "", "sandbox", false},
		{"darwin sandbox", "darwin", "sandbox", "sandbox", false},
		{"darwin full access", "darwin", "native-trusted", "native-trusted", false},
		{"windows default", "windows", "", "", true},
		{"windows sandbox", "windows", "sandbox", "", true},
		{"windows full access", "windows", "native-trusted", "native-trusted", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_site": "Example Project"})
			root := t.TempDir()
			config := filepath.Join(root, "config", "global.yaml")
			args := []string{"--url", hub.server.URL + "/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", config, "--workspace-root", filepath.Join(root, "work")}
			if test.selected != "" {
				args = append(args, "--isolation-tier", test.selected)
			}
			register := func(args ...string) (string, error) {
				starter := func(*cobra.Command, string) error { t.Error("unexpected service start"); return nil }
				command := newHubRunnerRegisterCommandWithReporter("test", test.goos, func(string) string { return "" }, starter, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil })
				return executeRegister(t, command, args...)
			}
			output, err := register(args...)
			if test.refused {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "sandbox is unavailable on this platform") || !strings.Contains(err.Error(), "--isolation-tier native-trusted") {
					t.Fatalf("register error=%v, output=%s", err, output)
				}
				if hub.requests.Load() != 0 {
					t.Fatal("unsupported tier contacted the Hub")
				}
				if _, err := os.Stat(filepath.Dir(config)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsupported tier created runner files: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("register: %v\n%s", err, output)
			}
			path := filepath.Join(filepath.Dir(config), "identity.json")
			cached, err := runnerauth.LoadRoutingCache(path)
			if err != nil || cached.Routing.IsolationTier != test.want || hub.tier.Load() != test.want {
				t.Fatalf("routing=%+v, Hub tier=%v, err=%v", cached, hub.tier.Load(), err)
			}
			before := mustRead(t, config)
			retryTier := "sandbox"
			if test.goos == "windows" {
				retryTier = "native-trusted"
			}
			if output, err := register(append(args, "--isolation-tier", retryTier)...); err != nil {
				t.Fatalf("retry: %v\n%s", err, output)
			}
			cached, err = runnerauth.LoadRoutingCache(path)
			if err != nil || cached.Routing.IsolationTier != test.want || hub.redeemed.Load() != 1 || mustRead(t, config) != before {
				t.Fatalf("retry changed existing configuration: %+v, %v", cached, err)
			}
		})
	}
}

func TestHubRunnerRegisterKeepsMachineSettings(t *testing.T) {
	t.Parallel()
	hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_site": "detent.build"})
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "global.yaml")
	paths := runnerPaths{config: configPath, identity: filepath.Join(root, "config", "identity.json"), workspaces: filepath.Join(root, "kept-root")}
	kept := runnerConfig(hub.server.URL+"/organizations/org_example", "org_example", "Build host", 2, paths)
	if _, err := writeRunnerConfig(configPath, kept); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, configPath)
	started := false
	output, err := runRegisterInTestWorkspace(t, nil, func(_ *cobra.Command, path string) error { started = true; return nil }, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", filepath.Join(root, "supplied-root"), "--service")
	if err != nil {
		t.Fatalf("register: %v: %s", err, output)
	}
	if !started || mustRead(t, configPath) != before {
		t.Fatalf("registration changed existing machine settings: %s", output)
	}
}

func checkout(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte("---\ntracker:\n  kind: memory\n  repository: acme/orders\n---\nWork the issue.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDoctorWorkflowSourceGit(t, dir, "init")
	runDoctorWorkflowSourceGit(t, dir, "add", "WORKFLOW.md")
	runDoctorWorkflowSourceGit(t, dir, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial workflow")
	runDoctorWorkflowSourceGit(t, dir, "remote", "add", "origin", "git@github.com:acme/orders.git")
}

func TestHubRunnerRegisterResumesEnrollment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                        string
		flagURL, retryFlagURL, fail bool
	}{
		{name: "organization URL", retryFlagURL: true},
		{name: "organization flag", flagURL: true},
		{name: "retry same flag command", flagURL: true, retryFlagURL: true, fail: true},
		{name: "retry same organization URL", fail: true},
		{name: "retry flag as organization URL", flagURL: true, fail: true},
		{name: "retry organization URL as flag", retryFlagURL: true, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, nil)
			hub.minimumVersion = "0.117.56"
			hub.failCapabilities.Store(test.fail)
			root := t.TempDir()
			configPath := filepath.Join(root, "config", "global.yaml")
			identityPath := filepath.Join(filepath.Dir(configPath), "identity.json")
			started := 0
			starter := func(_ *cobra.Command, path string) error {
				if path != configPath {
					t.Fatalf("service config = %q", path)
				}
				started++
				return nil
			}
			run := func(flagURL bool, token string) (string, error) {
				args := []string{"--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", filepath.Join(root, "work"), "--token", token, "--service"}
				if flagURL {
					args = append(args, "--url", hub.server.URL, "--organization", "org_example")
				} else {
					args = append(args, "--url", hub.server.URL+"/organizations/org_example")
				}
				return runRegisterInTestWorkspace(t, nil, starter, args...)
			}
			output, err := run(test.flagURL, "det_enroll_example")
			if test.fail {
				if err == nil || !strings.Contains(err.Error(), "read minimum runner version") || !strings.Contains(err.Error(), "503") || started != 0 {
					t.Fatalf("injected failure: %v, service starts %d", err, started)
				}
				if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("config after failure: %v", err)
				}
			} else if err != nil {
				t.Fatalf("register: %v\n%s", err, output)
			}
			before, err := runnerauth.Load(identityPath)
			if err != nil {
				t.Fatal(err)
			}
			hub.failCapabilities.Store(false)
			retryToken := "det_enroll_fresh"
			if test.flagURL == test.retryFlagURL {
				retryToken = "det_enroll_example"
			}
			output, err = run(test.retryFlagURL, retryToken)
			if err != nil {
				t.Fatalf("resume: %v\n%s", err, output)
			}
			after, err := runnerauth.Load(identityPath)
			if err != nil {
				t.Fatal(err)
			}
			if after.Identity.Binding != before.Identity.Binding || after.Credential != before.Credential || hub.redeemed.Load() != 1 {
				t.Fatalf("resume replaced identity or redeemed again (%d redemptions)", hub.redeemed.Load())
			}
			var result runnerRegistration
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			wantStarts := 2
			if test.fail {
				wantStarts = 1
			}
			if result.RunnerID != before.Identity.RunnerID || !result.ServiceRun || started != wantStarts || result.Created != test.fail {
				t.Fatalf("resume result = %+v, starts %d", result, started)
			}
			cfg, err := globalconfig.Read(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Client.URL != before.HubURL || cfg.Client.OrganizationID != "org_example" || cfg.Client.IdentityFile != identityPath {
				t.Fatalf("resumed config = %+v", cfg.Client)
			}
			if strings.Contains(output, before.Credential) || strings.Contains(output, retryToken) {
				t.Fatal("resume exposed a secret")
			}
		})
	}
}

func TestHubRunnerRegisterVersionGuidance(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, version, minimum string
		wantUpdate             bool
	}{
		{name: "older binary", version: "v0.117.55", minimum: "0.117.56", wantUpdate: true},
		{name: "minimum supported", version: "v0.117.56", minimum: "0.117.56"},
		{name: "newer binary", version: "v0.117.60", minimum: "0.117.56"},
		{name: "development binary", version: "dev", minimum: "0.117.56"},
		{name: "Hub without a minimum", version: "v0.117.55"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
			hub.minimumVersion = test.minimum
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			command := newHubRunnerRegisterCommandWithReporter(test.version, "linux", func(string) string { return "" }, func(*cobra.Command, string) error {
				t.Fatal("service started without a checkout")
				return nil
			}, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil })
			output, err := executeRegister(t, command, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", filepath.Join(root, "global.yaml"), "--workspace-root", filepath.Join(root, "work"))
			if err != nil {
				t.Fatal(err)
			}
			var registration runnerRegistration
			if err := json.Unmarshal([]byte(output), &registration); err != nil {
				t.Fatal(err)
			}
			steps := strings.Join(registration.NextSteps, "\n")
			if strings.Contains(steps, "detent update --yes --from-release") != test.wantUpdate {
				t.Fatalf("update guidance = %q, want update %v", steps, test.wantUpdate)
			}
			if test.wantUpdate && (!strings.Contains(steps, "requires Detent 0.117.56 or newer") || !strings.Contains(steps, test.version) || !strings.Contains(steps, "brew upgrade")) {
				t.Fatalf("missing version or update guidance: %s", steps)
			}
			if strings.Contains(output, "det_enroll_example") {
				t.Fatal("registration output exposed the token")
			}
		})
	}
}

func TestHubRunnerRegisterRejectsBadInput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	never := func(*cobra.Command, string) error {
		t.Error("service started for rejected input")
		return nil
	}
	base := []string{"--url", "https://hub.example.test/organizations/org_example"}
	for _, test := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{name: "no token", args: append(base, "--config", filepath.Join(root, "a", "global.yaml")), want: "enrollment token is required"},
		{name: "zero capacity", args: append(base, "--token", "t", "--capacity", "0", "--config", filepath.Join(root, "b", "global.yaml")), want: "--capacity"},
		{name: "unknown tier", args: append(base, "--token", "t", "--isolation-tier", "root"), want: "--isolation-tier"},
		{name: "relative config", args: append(base, "--token", "t", "--config", "global.yaml"), want: "absolute"},
		{name: "config inside a repository", args: append(base, "--token", "t", "--config", filepath.Join(repository, "runner", "global.yaml")), want: "outside repositories"},
		{name: "no organization", args: []string{"--url", "https://hub.example.test", "--token", "t"}, want: "organization is unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			output, err := runRegister(t, test.env, never, test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q\n%s", err, test.want, output)
			}
		})
	}
}

func TestHubRunnerRegisterRefusesAnotherRunnersFiles(t *testing.T) {
	t.Parallel()
	hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_site": "detent.build"})
	url := hub.server.URL + "/organizations/org_example"
	never := func(*cobra.Command, string) error { return nil }

	for _, test := range []struct {
		name, hubURL, organization, wantError string
	}{
		{name: "identity for another hub", hubURL: "https://other-hub.example.test", wantError: "other-hub.example.test"},
		{name: "identity for another organization", hubURL: hub.server.URL, organization: "org_other", wantError: "org_other"},
		{name: "identity URL for another organization", hubURL: hub.server.URL + "/organizations/org_other", organization: "org_example", wantError: "org_other"},
		{name: "identity for another base path", hubURL: hub.server.URL + "/other", organization: "org_example", wantError: "/other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(t.TempDir(), "runner", "global.yaml")
			identityPath := filepath.Join(filepath.Dir(configPath), "identity.json")
			file, err := runnerauth.Initialize(identityPath, test.hubURL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = tracker.OrganizationID(test.organization)
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			before := mustRead(t, identityPath)
			_, err = runRegisterInTestWorkspace(t, nil, never, "--url", url, "--token", "det_enroll_example", "--config", configPath, "--workspace-root", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("err = %v, want refusal naming %q", err, test.wantError)
			}
			if mustRead(t, identityPath) != before || hub.redeemed.Load() != 0 {
				t.Fatal("refusal changed identity or redeemed token")
			}
		})
	}

	t.Run("config for another organization", func(t *testing.T) {
		t.Parallel()
		configPath := filepath.Join(t.TempDir(), "runner", "global.yaml")
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		existing := "client:\n  hub_url: " + url + "\n  organization_id: org_other\n  identity_file: " + filepath.Join(filepath.Dir(configPath), "identity.json") + "\n"
		if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := runRegisterInTestWorkspace(t, nil, never, "--url", url, "--token", "det_enroll_example", "--config", configPath, "--workspace-root", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "client.organization_id") {
			t.Fatalf("err = %v, want a refusal naming the mismatched key", err)
		}
		if mustRead(t, configPath) != existing || hub.redeemed.Load() != 0 {
			t.Fatalf("register rewrote the configuration or spent the token (%d redemptions)", hub.redeemed.Load())
		}
	})
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestHubRunnerRegisterReportsBeforeService(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name       string
		credential string
		envToken   string
		authErr    error
		wantChecks int
		wantLogin  bool
	}{
		{name: "signed in", wantChecks: 1},
		{name: "signed out still starts service", authErr: errors.New("private credential output"), wantChecks: 1, wantLogin: true},
		{name: "kept custom credential", credential: "custom-token", authErr: errors.New("signed out")},
		{name: "environment credential", envToken: "environment-token", authErr: errors.New("signed out")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
			hub.repositories["prj_orders"] = "acme/orders"
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "global.yaml")
			workspaces := filepath.Join(root, "work")
			workdir := filepath.Join(workspaces, "orders")
			checkout(t, workdir)
			if err := os.WriteFile(filepath.Join(workdir, "WORKFLOW.md"), []byte("Work the issue.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workdir, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: hub_native\ndeliverable:\n  github_pull_request: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runDoctorWorkflowSourceGit(t, workdir, "config", "url."+workdir+".insteadOf", "git@github.com:acme/orders.git")
			runDoctorWorkflowSourceGit(t, workdir, "add", "WORKFLOW.md", "detent.yaml")
			runDoctorWorkflowSourceGit(t, workdir, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "GitHub workflow")
			var before string
			if test.credential != "" {
				paths := runnerPaths{config: configPath, identity: filepath.Join(root, "identity.json"), workspaces: workspaces}
				kept := runnerConfig(hub.server.URL+"/organizations/org_example", "org_example", "Build host", 2, paths)
				kept.GitHubToken = test.credential
				if _, err := writeRunnerConfig(configPath, kept); err != nil {
					t.Fatal(err)
				}
				before = mustRead(t, configPath)
			}
			reported, started := false, false
			reporter := func(_ context.Context, cfg globalconfig.Config, version string) error {
				if _, err := os.Stat(cfg.Path); err != nil {
					t.Fatal("diagnostics ran before config was written", err)
				}
				if version != "test" || cfg.WorkspaceRoot != workspaces || cfg.Client.NativeProjects["orders"] != "prj_orders" {
					t.Fatalf("wrong diagnostic context: %+v", cfg)
				}
				reported = true
				return nil
			}
			starter := func(_ *cobra.Command, path string) error {
				if !reported {
					t.Fatal("service started before diagnostics")
				}
				started = true
				return nil
			}
			checks := 0
			cmd := newHubRunnerRegisterCommandWithServiceName("test", "linux", mapLookup(map[string]string{"GITHUB_TOKEN": test.envToken}), starter, func(string) error { return nil }, reporter, func(context.Context) error {
				checks++
				return test.authErr
			}, testRunnerServiceName)
			output, err := executeRegister(t, cmd, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaces, "--service")
			if err != nil || !reported || !started || checks != test.wantChecks {
				t.Fatalf("reported=%v started=%v checks=%d err=%v", reported, started, checks, err)
			}
			var registration runnerRegistration
			if err := json.Unmarshal([]byte(output), &registration); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.Join(registration.NextSteps, "\n"), "Project orders needs GitHub authentication: run gh auth login") != test.wantLogin || strings.Contains(output, "private credential output") {
				t.Fatalf("next steps = %v", registration.NextSteps)
			}
			if test.credential != "" && mustRead(t, configPath) != before {
				t.Fatal("registration rewrote the operator's credential setting")
			}
			if got, err := cmd.Flags().GetInt("capacity"); err != nil || cmd.Flags().Lookup("capacity").DefValue != "1" || got != 2 {
				t.Fatalf("capacity=%d %v", got, err)
			}
		})
	}
}
