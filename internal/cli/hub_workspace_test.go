package cli

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cli/remoterunner"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func newWorkspaceLaneTestScheduler(t *testing.T, projects map[string]string) orchestrator.SchedulingSource {
	t.Helper()
	transport := readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet && request.URL.Path == "/api/v2/organizations/org_test/projects/prj_test" {
			_, _ = response.WriteString(`{}`)
			return response.Result(), nil
		}
		if request.Method == http.MethodGet && request.URL.Path == "/api/v2/organizations/org_test/projects/prj_test/policy" {
			_, _ = response.WriteString(`{"policy":null}`)
			return response.Result(), nil
		}
		response.WriteHeader(http.StatusNotFound)
		return response.Result(), nil
	})
	client, err := hubclient.New(hubclient.Config{URL: "https://hub.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native := make(map[string]tracker.ProjectID, len(projects))
	for name, id := range projects {
		native[name] = tracker.ProjectID(id)
	}
	scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		OrganizationID: "org_test", NativeProjects: native,
		Machine:           hubclient.Machine{ID: "machine_1", Hostname: "host", Capacity: 2, Version: "test"},
		HeartbeatInterval: 20 * time.Second, LeaseTTL: 75 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

// writeWorkspaceLaneProject writes a workflow a policy can be resolved from.
func writeWorkspaceLaneProject(t *testing.T, projectID string) globalconfig.Project {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "WORKFLOW.md")
	raw := "---\ntracker:\n  kind: hub_native\n  repository: example-org/" + projectID +
		"\n  api_key: test-token\nworkspace:\n  root: " + filepath.Join(root, "worktrees") + "\n---\nPrivate instructions.\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return globalconfig.Project{ID: projectID, Workflow: path, Workdir: root}
}

func TestNewWorkspaceLanes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// nativeProject is the hub project the runner is configured for.
		nativeProject string
		// scheduling replaces the hub scheduler when it is not nil, to stand
		// for a runner scheduling through something else entirely.
		scheduling func(t *testing.T) orchestrator.SchedulingSource
		// anonymous leaves the runner without an enrolled identity.
		anonymous bool
		want      int
	}{
		{name: "a configured project gets a lane", nativeProject: "orders", want: 1},
		// The project is enrolled with the hub but is not one this runner
		// runs, so there is no workflow to resolve a policy from. The lane is
		// skipped rather than failing the daemon.
		{name: "an unconfigured project is skipped", nativeProject: "unknown", want: 0},
		{name: "no hub scheduler starts no lane", nativeProject: "orders", want: 0,
			scheduling: func(*testing.T) orchestrator.SchedulingSource { return nil }},
		{name: "a runner without an enrolled identity starts no lane", nativeProject: "orders", anonymous: true, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Config{
				Projects: []globalconfig.Project{writeWorkspaceLaneProject(t, "orders")},
			}
			cfg.Client.NativeProjects = map[string]string{test.nativeProject: "prj_test"}
			if !test.anonymous {
				cfg.Client.IdentityFile = filepath.Join(t.TempDir(), "runner.json")
			}
			scheduling := newWorkspaceLaneTestScheduler(t, cfg.Client.NativeProjects)
			if test.scheduling != nil {
				scheduling = test.scheduling(t)
			}
			lanes := remoterunner.NewWorkspaceLanes(t.Context(), cfg, scheduling, "1", slog.New(slog.DiscardHandler), testWorkspaceSupport(), buildWorkspaceBackendContext)
			if len(lanes) != test.want {
				t.Fatalf("newWorkspaceLanes() returned %d lanes, want %d", len(lanes), test.want)
			}
		})
	}
}

func TestWorkspaceLaneCapabilities(t *testing.T) {
	t.Parallel()
	served := workspacerunner.Capabilities(testWorkspaceSupport())
	withoutGit := served
	withoutGit.Git = false
	tests := []struct {
		name     string
		identity string
		token    string
		projects map[string]string
		kind     string
		want     workspacesession.Capabilities
	}{
		{name: "an enrolled runner on git worktrees reports what it serves", identity: "/runner/identity.json", projects: map[string]string{"orders": "prj_test"}, want: served},
		{name: "an enrolled runner on filesystem worktrees reports no git", identity: "/runner/identity.json", projects: map[string]string{"orders": "prj_test"}, kind: "filesystem", want: withoutGit},
		{name: "a project the runner cannot resolve reports no git", identity: "/runner/identity.json", projects: map[string]string{"unknown": "prj_test"}, want: withoutGit},
		{name: "a token runner reports nothing", token: "HUB_TOKEN", projects: map[string]string{"orders": "prj_test"}},
		{name: "an enrolled runner without native projects reports nothing", identity: "/runner/identity.json"},
		{name: "a blank identity path reports nothing", identity: "  ", projects: map[string]string{"orders": "prj_test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			orders := writeWorkspaceLaneProject(t, "orders")
			if test.kind != "" {
				raw, err := os.ReadFile(orders.Workflow)
				if err != nil {
					t.Fatal(err)
				}
				updated := strings.Replace(string(raw), "workspace:\n", "workspace:\n  kind: "+test.kind+"\n", 1)
				if err := os.WriteFile(orders.Workflow, []byte(updated), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := globalconfig.Config{Projects: []globalconfig.Project{orders}}
			cfg.Client = globalconfig.HubClient{IdentityFile: test.identity, TokenEnvironment: test.token, NativeProjects: test.projects}
			if got := remoterunner.WorkspaceLaneCapabilities(t.Context(), cfg, testWorkspaceSupport()); got != test.want {
				t.Fatalf("workspaceLaneCapabilities() = %+v, want %+v", got, test.want)
			}
		})
	}
}
