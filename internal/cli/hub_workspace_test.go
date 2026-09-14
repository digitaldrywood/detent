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

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The runner's workspace lane wiring (decisions section 18.1). A workspace is
// claimed only by a runner that reports the capabilities it requires, so the
// decision to run a lane at all and the report on the heartbeat answer one
// question and must agree.

func TestWorkspaceLaneEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		capabilities workspacesession.Capabilities
		want         bool
	}{
		{"files reported", workspacesession.Capabilities{Files: true}, true},
		{"files with diff reported", workspacesession.Capabilities{Files: true, Diff: true}, true},
		{"nothing reported", workspacesession.Capabilities{}, false},
		// A request without `requires` gets files and diff, so a runner that
		// reports only a terminal would still be refused every claim.
		{"terminal only", workspacesession.Capabilities{Terminal: true}, false},
		{"diff only", workspacesession.Capabilities{Diff: true}, false},
		// The same holds for exec (section 18.12): serving project actions is
		// no use to a claim that asks for files, and serving files is useful
		// on its own to a reader who runs no action at all.
		{"files with exec reported", workspacesession.Capabilities{Files: true, Exec: true}, true},
		{"exec only", workspacesession.Capabilities{Exec: true}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workspaceLaneEnabled(test.capabilities); got != test.want {
				t.Errorf("workspaceLaneEnabled(%#v) = %t, want %t", test.capabilities, got, test.want)
			}
		})
	}
}

func TestWorkspaceSessionIDsAreUniqueAndDistinct(t *testing.T) {
	t.Parallel()
	next := workspaceSessionIDs("machine_1")
	seen := map[string]bool{}
	for range 3 {
		id, err := next()
		if err != nil {
			t.Fatalf("workspaceSessionIDs() error = %v", err)
		}
		if seen[id] {
			t.Fatalf("session id %q was handed out twice", id)
		}
		// The hub pins a policy per lease session, so a workspace claim must
		// never reuse the issue lane's session.
		if !strings.HasPrefix(id, "machine_1-workspace-") {
			t.Fatalf("session id %q does not name the workspace lane", id)
		}
		seen[id] = true
	}
}

// newWorkspaceLaneTestScheduler builds a scheduler over a hub that is never
// called: newWorkspaceLanes talks to no one, it only wires.
func newWorkspaceLaneTestScheduler(t *testing.T, projects map[string]string) orchestrator.SchedulingSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
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
	raw := "---\ntracker:\n  kind: github\n  github_status_source: label\n  repository: acme/" + projectID +
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
		want       int
	}{
		{name: "a configured project gets a lane", nativeProject: "orders", want: 1},
		// The project is enrolled with the hub but is not one this runner
		// runs, so there is no workflow to resolve a policy from. The lane is
		// skipped rather than failing the daemon.
		{name: "an unconfigured project is skipped", nativeProject: "unknown", want: 0},
		{name: "no hub scheduler starts no lane", nativeProject: "orders", want: 0,
			scheduling: func(*testing.T) orchestrator.SchedulingSource { return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Config{
				Projects: []globalconfig.Project{writeWorkspaceLaneProject(t, "orders")},
			}
			cfg.Client.NativeProjects = map[string]string{test.nativeProject: "prj_test"}
			scheduling := newWorkspaceLaneTestScheduler(t, cfg.Client.NativeProjects)
			if test.scheduling != nil {
				scheduling = test.scheduling(t)
			}
			lanes := newWorkspaceLanes(t.Context(), cfg, scheduling, slog.New(slog.DiscardHandler))
			if len(lanes) != test.want {
				t.Fatalf("newWorkspaceLanes() returned %d lanes, want %d", len(lanes), test.want)
			}
		})
	}
}
