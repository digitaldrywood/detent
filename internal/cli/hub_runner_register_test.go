package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
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
		{name: "missing configured workflow despite default", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, missingWorkflow: true},
		{name: "invalid configured workflow despite default", remote: "git@github.com:Acme/Private.git", externalWorkflow: true, invalidWorkflow: true},
		{name: "missing default workflow", remote: "git@github.com:Acme/Private.git", missingWorkflow: true},
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

func TestRunnerProjectSlug(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, want string }{
		{name: "detent.build", want: "detent.build"},
		{name: "Client Portal", want: "client-portal"},
		{name: "  Ops / Infra  ", want: "ops-infra"},
		{name: "snake_case", want: "snake_case"},
		{name: "!!!", want: "prj_fallback"},
	} {
		if got := runnerProjectSlug(test.name, "prj_fallback"); got != test.want {
			t.Errorf("runnerProjectSlug(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

type registerHub struct {
	server     *httptest.Server
	credential atomic.Value
	identity   atomic.Value
	redeemed   atomic.Int32
	projects   map[tracker.ProjectID]string
}

func newRegisterHub(t *testing.T, projects map[tracker.ProjectID]string) *registerHub {
	t.Helper()
	hub := &registerHub{projects: projects}
	hub.credential.Store("")
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasPrefix(r.URL.Path, "/organizations/org_example/api/v2/organizations/org_example/") {
			t.Errorf("request outside the organization: %s", r.URL.Path)
		}
		ids := make([]tracker.ProjectID, 0, len(projects))
		for id := range projects {
			ids = append(ids, id)
		}
		if strings.HasSuffix(r.URL.Path, "/runner-enrollments/redeem") {
			if r.Header.Get("Authorization") != "Bearer det_enroll_example" {
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
		for id, name := range projects {
			if strings.HasSuffix(r.URL.Path, "/projects/"+string(id)) {
				_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: id, OrganizationID: "org_example", Name: name})
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
	return executeRegister(t, newHubRunnerRegisterCommandWithReporter("test", func(name string) string { return env[name] }, starter, runnerPrivateLocation, func(context.Context, globalconfig.Config, string) error { return nil }), args...)
}

// Registration fixtures stay under t.TempDir even when worker TMPDIR is inside this repository.
func runRegisterInTestWorkspace(t *testing.T, env map[string]string, starter runnerServiceStarter, args ...string) (string, error) {
	t.Helper()
	return executeRegister(t, newHubRunnerRegisterCommandWithReporter("test", func(name string) string { return env[name] }, starter, func(string) error { return nil }, func(context.Context, globalconfig.Config, string) error { return nil }), args...)
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
	steps := strings.Join(registration.NextSteps, "\n")
	if len(started) != 0 || !strings.Contains(steps, "Clone the ops-tools repository into "+filepath.Join(workspaces, "ops-tools")) || !strings.Contains(steps, "detent start --config") {
		t.Fatalf("service started before every checkout exists (%v):\n%s", started, output)
	}

	var written runnerConfigFile
	if err := yaml.Unmarshal([]byte(mustRead(t, configPath)), &written); err != nil || written.Client.NativeProjects["ops-tools"] != "prj_ops" {
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
	if len(started) != 1 || started[0] != configPath || rerun.Created || !rerun.ServiceRun || !strings.HasPrefix(mustRead(t, configPath), "# edited by the operator") {
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
	if cfg.Client.NativeProjects["detent.build"] != "prj_site" || cfg.Client.NativeProjects["ops-tools"] != "prj_ops" || len(cfg.Projects) != 2 {
		t.Fatalf("projects = %+v, native = %v", cfg.Projects, cfg.Client.NativeProjects)
	}
	for _, project := range cfg.Projects {
		if project.Workdir != filepath.Join(workspaces, project.ID) || project.Workflow != filepath.Join(project.Workdir, "WORKFLOW.md") {
			t.Fatalf("project entry = %+v", project)
		}
	}
	if file.Identity.OrganizationID != "org_example" || !strings.Contains(output, file.Identity.RunnerID) {
		t.Fatalf("identity = %+v", file.Identity)
	}
}

func TestHubRunnerRegisterChecksKeptProjectWorkdirs(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name            string
		ready           bool
		workspaceRoot   bool
		absentPaths     bool
		relativeWorkdir bool
		missingWorkflow bool
	}{
		{name: "kept checkout with default root", ready: true},
		{name: "kept checkout overrides supplied root", ready: true, workspaceRoot: true},
		{name: "missing kept checkout ignores supplied checkout", workspaceRoot: true},
		{name: "absent kept workdir and workflow", absentPaths: true},
		{name: "absent kept paths with relative workdir", absentPaths: true, relativeWorkdir: true},
		{name: "kept workdir without workflow", missingWorkflow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_site": "detent.build"})
			root := t.TempDir()
			configPath := filepath.Join(root, "config", "global.yaml")
			workdir := filepath.Join(root, "actual-checkout")
			paths := runnerPaths{config: configPath, identity: filepath.Join(root, "config", "identity.json"), workspaces: filepath.Join(root, "unused-root")}
			kept := runnerConfig(hub.server.URL+"/organizations/org_example", "org_example", "Build host", 2, paths, []runnerRegisteredCheck{{Name: "local-project", ID: "prj_site", Workdir: workdir}})
			if test.relativeWorkdir {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				// This case only inspects an absent checkout. Keep its relative path
				// on the current drive when Windows scratch lives on another drive.
				kept.Projects[0].Workdir = filepath.Join("missing-checkout", filepath.Base(filepath.Dir(root)))
				workdir = filepath.Join(cwd, kept.Projects[0].Workdir)
			}
			if _, err := writeRunnerConfig(configPath, kept); err != nil {
				t.Fatal(err)
			}
			kept.ServiceName = "" // Runner configs predating service_name remain supported.
			body, err := yaml.Marshal(kept)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if !test.absentPaths {
				checkout(t, workdir)
			}
			if !test.ready && !test.absentPaths {
				if err := os.RemoveAll(filepath.Join(workdir, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if test.missingWorkflow {
				if err := os.Remove(filepath.Join(workdir, "WORKFLOW.md")); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--url", hub.server.URL + "/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--service"}
			if test.workspaceRoot {
				checkout(t, filepath.Join(paths.workspaces, "detent.build"))
				args = append(args, "--workspace-root", paths.workspaces)
			}
			started := false
			output, err := runRegisterInTestWorkspace(t, nil, func(_ *cobra.Command, path string) error { started = true; return nil }, args...)
			if err != nil {
				t.Fatalf("register: %v\n%s", err, output)
			}
			var result runnerRegistration
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}
			if started != test.ready || result.ServiceRun != test.ready || len(result.Projects) != 1 || result.Projects[0].Workdir != workdir || result.Projects[0].Checkout != test.ready {
				t.Fatalf("started %v, registration %+v", started, result)
			}
			if !test.ready {
				steps := strings.Join(result.NextSteps, "\n")
				if !strings.Contains(steps, "Clone the local-project repository into "+workdir) || !strings.Contains(steps, "detent start --config "+shellQuote(configPath)+" --yes") {
					t.Fatalf("next steps = %v", result.NextSteps)
				}
			}
			if mustRead(t, configPath) != string(body) {
				t.Fatal("kept config was rewritten")
			}
		})
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

	t.Run("identity for another hub", func(t *testing.T) {
		t.Parallel()
		configPath := filepath.Join(t.TempDir(), "runner", "global.yaml")
		if _, err := runnerauth.Initialize(filepath.Join(filepath.Dir(configPath), "identity.json"), "https://other-hub.example.test"); err != nil {
			t.Fatal(err)
		}
		_, err := runRegisterInTestWorkspace(t, nil, never, "--url", url, "--token", "det_enroll_example", "--config", configPath, "--workspace-root", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "other-hub.example.test") {
			t.Fatalf("err = %v, want a refusal naming the other hub", err)
		}
	})

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
	hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "global.yaml")
	workspaces := filepath.Join(root, "work")
	checkout(t, filepath.Join(workspaces, "orders"))
	reported := false
	reporter := func(_ context.Context, cfg globalconfig.Config, version string) error {
		if _, err := os.Stat(cfg.Path); err != nil {
			t.Fatal("diagnostics ran before config was written", err)
		}
		if version != "test" || cfg.Client.NativeProjects["orders"] != "prj_orders" {
			t.Fatalf("wrong diagnostic context: %+v", cfg)
		}
		reported = true
		return nil
	}
	starter := func(_ *cobra.Command, path string) error {
		if !reported {
			t.Fatal("service started before diagnostics")
		}
		return nil
	}
	cmd := newHubRunnerRegisterCommandWithReporter("test", func(string) string { return "" }, starter, func(string) error { return nil }, reporter)
	_, err := executeRegister(t, cmd, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaces, "--service")
	if err != nil || !reported {
		t.Fatalf("reported=%v err=%v", reported, err)
	}
	if got, err := cmd.Flags().GetInt("capacity"); err != nil || cmd.Flags().Lookup("capacity").DefValue != "1" || got != 2 {
		t.Fatalf("capacity=%d %v", got, err)
	}
}
