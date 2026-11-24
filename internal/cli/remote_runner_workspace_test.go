package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRemoteRunnerWorkspaceLane(t *testing.T) {
	var handler atomic.Pointer[http.HandlerFunc]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load())(w, r)
	}))
	t.Cleanup(server.Close)
	identityPath := filepath.Join(t.TempDir(), "private", "runner.json")
	identity, err := runnerauth.Initialize(identityPath, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	identity.Identity.OrganizationID = "org_test"
	identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
	identity.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(identityPath, identity); err != nil {
		t.Fatal(err)
	}

	var previousSession string
	for _, test := range []struct {
		name   string
		serves bool
	}{
		{name: "unresolvable workspace workflow"},
		{name: "workspace served", serves: true},
		{name: "workspace served after restart", serves: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			selected := writeWorkspaceLaneProject(t, "example")
			selected.Weight = 1
			runDoctorWorkflowSourceGit(t, selected.Workdir, "init")
			runDoctorWorkflowSourceGit(t, selected.Workdir, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "Initial commit")
			head := runDoctorWorkflowSourceGit(t, selected.Workdir, "rev-parse", "HEAD")
			if !test.serves {
				selected.Workflow = filepath.Join(root, "missing", "WORKFLOW.md")
			}

			ticks := make(chan time.Time, 2)
			tick := func() { ticks <- time.Now() }
			tick()
			hub, serveHub := newRemoteWorkspaceHub(t, identity.Identity, head, test.serves, tick)
			handler.Store(&serveHub)
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "runner.yaml"), globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Path = ""
			cfg.Projects = []globalconfig.Project{selected}
			cfg.Client = globalconfig.HubClient{
				URL: server.URL, IdentityFile: identityPath, OrganizationID: "org_test",
				NativeProjects: map[string]string{selected.ID: "prj_test"}, HeartbeatIntervalSeconds: 1,
				ProviderCapacityFile: writeProviderCapacityFixture(t, root),
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			statePath := filepath.Join(root, "runtime.db")
			go func() {
				defer close(done)
				runErr = startRemoteRunner(ctx, BootConfig{
					Global: cfg, Headless: true, Version: "dev", RuntimeDBPath: statePath,
					Shutdown: NewShutdownController(),
					remoteHeartbeatTicker: func(time.Duration) (<-chan time.Time, func()) {
						return ticks, func() {}
					},
				})
			}()
			t.Cleanup(func() {
				cancel()
				hub.allowUnbind()
				<-done
			})
			waitCtx := testenv.WaitContext(t, t.Context())
			want := workspacesession.Capabilities{}
			if test.serves {
				want = workspacerunner.Capabilities(workspacerunner.DefaultSupport())
			}
			for {
				got := awaitRemoteRunnerEvent(t, waitCtx, done, hub.reports)
				if got == nil {
					t.Fatal("heartbeat omitted workspace capabilities")
				}
				if *got == want {
					break
				}
				if !test.serves || *got != (workspacesession.Capabilities{}) {
					t.Fatalf("heartbeat capabilities = %+v, want %+v", *got, want)
				}
			}
			if test.serves {
				issueSession := awaitRemoteRunnerEvent(t, waitCtx, done, hub.issueClaim)
				claimSession := awaitRemoteRunnerEvent(t, waitCtx, done, hub.workspaceClaim)
				if claimSession == "" || issueSession == "" || claimSession == previousSession || claimSession == issueSession {
					t.Fatalf("workspace session %q was empty or reused (previous %q, issue %q)", claimSession, previousSession, issueSession)
				}
				previousSession = claimSession
				awaitRemoteRunnerEvent(t, waitCtx, done, hub.ready)
				select {
				case <-hub.unbinding:
					t.Fatal("workspace unbound before runner cancellation")
				default:
				}
			}

			cancel()
			if test.serves {
				awaitRemoteRunnerEvent(t, waitCtx, done, hub.unbinding)
				hub.allowUnbind()
			}
			<-done
			if test.serves {
				select {
				case <-hub.released:
				default:
					t.Fatal("runner stopped before releasing the workspace claim")
				}
			}
			if !errors.Is(runErr, context.Canceled) {
				t.Fatalf("runner shutdown = %v, want context cancellation", runErr)
			}
			if _, err := os.Stat(statePath); !os.IsNotExist(err) {
				t.Fatalf("runner created local runtime state: %v", err)
			}
		})
	}
}

func awaitRemoteRunnerEvent[T any](t *testing.T, ctx context.Context, done <-chan struct{}, event <-chan T) T {
	t.Helper()
	select {
	case value := <-event:
		return value
	case <-done:
		t.Fatal("runner stopped before the Hub event")
	case <-ctx.Done():
		t.Fatal("Hub event was not observed before the test deadline")
	}
	var zero T
	return zero
}

type remoteWorkspaceHub struct {
	reports        chan *workspacesession.Capabilities
	issueClaim     chan string
	workspaceClaim chan string
	ready          chan struct{}
	unbinding      chan struct{}
	released       chan struct{}
	allowUnbind    func()
}

func newRemoteWorkspaceHub(t *testing.T, identity runnerauth.Identity, head string, offerWorkspace bool, requestHeartbeat func()) (*remoteWorkspaceHub, http.HandlerFunc) {
	t.Helper()
	const workspaceID = "ws_000000000000000000000000000000ff"
	reported := make(chan struct{})
	issueDispatched := make(chan struct{})
	unblock := make(chan struct{})
	hub := &remoteWorkspaceHub{
		reports:    make(chan *workspacesession.Capabilities, 8),
		issueClaim: make(chan string, 1), workspaceClaim: make(chan string, 1),
		ready: make(chan struct{}, 1), unbinding: make(chan struct{}, 1), released: make(chan struct{}, 1),
		allowUnbind: sync.OnceFunc(func() { close(unblock) }),
	}
	reportServing := sync.OnceFunc(func() { close(reported) })
	firstWorkspaceClaim := sync.OnceFunc(requestHeartbeat)
	claimOffered := atomic.Bool{}
	var firstIssueClaim sync.Once
	sendIssueClaim := func(session string) {
		firstIssueClaim.Do(func() {
			hub.issueClaim <- session
			close(issueDispatched)
		})
	}
	session := workspacesession.Session{ID: workspaceID, State: workspacesession.StateRequested}
	serve := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response any
		switch {
		case r.URL.Path == "/api/v2/capabilities":
			response = map[string]any{"version": "dev", "protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{
				"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeDispatchPriorityCapability,
				tracker.NativeWorkspaceCapability, tracker.NativeRunnerUpdateCapability, tracker.NativeProviderCapacityCapability, tracker.NativeLandingCapacityCapability,
			}}
		case strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID):
			response = identity
		case strings.HasSuffix(r.URL.Path, "/update/receipt"):
			response = (*runnerauth.UpdateReceipt)(nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_test"):
			response = tracker.NativeProject{Profile: "native"}
		case strings.HasSuffix(r.URL.Path, "/policy/observed"):
			w.WriteHeader(http.StatusNoContent)
			return
		case strings.HasSuffix(r.URL.Path, "/policy"):
			response = map[string]any{"policy": nil}
		case strings.HasSuffix(r.URL.Path, "/routing"):
			response = runnerauth.Runner{Binding: identity.Binding, HostCapacity: 1, ReportedCapacity: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 1}.Normalized()}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-items"):
			response = tracker.Page[tracker.NativeIssue]{}
		case strings.HasSuffix(r.URL.Path, "/claims/preview"):
			var preview tracker.NativeCapacityPreview
			decodeWorkspaceHubRequest(t, r, &preview)
			sendIssueClaim(preview.SessionID)
			response = tracker.NativeCapacityPage{}
		case strings.HasSuffix(r.URL.Path, "/claims"):
			var claim tracker.NativeClaim
			decodeWorkspaceHubRequest(t, r, &claim)
			if slices.Contains(claim.Capabilities, tracker.NativeWorkspaceCapability) {
				firstWorkspaceClaim()
				if !offerWorkspace {
					t.Error("runner attempted a workspace claim without a usable workflow")
				}
				if claim.TTLSeconds != int64(workspacesession.LeaseTTL/time.Second) {
					t.Errorf("workspace claim TTL = %d", claim.TTLSeconds)
				}
				select {
				case <-reported:
				case <-r.Context().Done():
					return
				}
				select {
				case <-issueDispatched:
				case <-r.Context().Done():
					return
				}
				if claimOffered.CompareAndSwap(false, true) {
					hub.workspaceClaim <- claim.SessionID
					response = tracker.NativeLease{ID: "lease_workspace", WorkItemID: "wi_workspace", FencingToken: 1, IsolationPolicy: &isolation.Policy{}}
					break
				}
			} else {
				sendIssueClaim(claim.SessionID)
			}
			w.WriteHeader(http.StatusConflict)
			response = map[string]string{"code": "no_claimable_work"}
		case strings.HasSuffix(r.URL.Path, "/work-items/wi_workspace/workspace"):
			response = session
		case strings.HasSuffix(r.URL.Path, "/workspaces/"+workspaceID+"/worker/bind"):
			var bind hubclient.WorkspaceBindRequest
			decodeWorkspaceHubRequest(t, r, &bind)
			if want := workspacerunner.Capabilities(workspacerunner.DefaultSupport()); bind.Capabilities != want {
				t.Errorf("workspace bind capabilities = %+v, want %+v", bind.Capabilities, want)
			}
			response = hubclient.WorkspaceBindResponse{Session: session, Checkout: hubclient.WorkspaceCheckout{
				WorkItemID: "wi_workspace", Worktree: workspacesession.WorktreeFresh, HeadSHA: head, ReadOnly: true,
			}}
		case strings.HasSuffix(r.URL.Path, "/workspaces/"+workspaceID+"/worker/heartbeat"):
			var heartbeat hubclient.WorkspaceHeartbeatRequest
			decodeWorkspaceHubRequest(t, r, &heartbeat)
			if heartbeat.State == workspacesession.StateReady {
				if !heartbeat.Capabilities.Git {
					t.Error("workspace did not report a Git worktree")
				}
				select {
				case hub.ready <- struct{}{}:
				default:
				}
			}
			response = hubclient.WorkspaceHeartbeatResponse{Session: session}
		case strings.HasSuffix(r.URL.Path, "/workspaces/"+workspaceID+"/worker/relay"):
			<-r.Context().Done()
			return
		case strings.HasSuffix(r.URL.Path, "/workspaces/"+workspaceID+"/worker/unbind"):
			hub.unbinding <- struct{}{}
			<-unblock
			w.WriteHeader(http.StatusNoContent)
			return
		case strings.HasSuffix(r.URL.Path, "/leases/lease_workspace/release"):
			w.WriteHeader(http.StatusNoContent)
			hub.released <- struct{}{}
			return
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var report struct {
				Capabilities *workspacesession.Capabilities `json:"workspace_capabilities"`
			}
			decodeWorkspaceHubRequest(t, r, &report)
			if report.Capabilities != nil && report.Capabilities.Files {
				reportServing()
			}
			select {
			case hub.reports <- report.Capabilities:
			default:
			}
			response = runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 1}.Normalized()}
		default:
			t.Logf("unhandled fake Hub request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}
	return hub, serve
}

func decodeWorkspaceHubRequest(t *testing.T, r *http.Request, dest any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		t.Errorf("decode %s: %v", r.URL.Path, err)
	}
}
