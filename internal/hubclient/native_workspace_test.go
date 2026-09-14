package hubclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace worker endpoints (decisions section 18.1). Every call carries
// the workspace owner tuple, and the one refusal a session acts on is
// stale_execution: it means this runner no longer owns the workspace and must
// stop serving it.

const testWorkspaceID = "ws_000000000000000000000000000000ff"

// workspaceHub records what the client asked for and answers with whatever the
// test scripted.
type workspaceHub struct {
	paths    []string
	binds    []WorkspaceBindRequest
	beats    []WorkspaceHeartbeatRequest
	unbinds  []WorkspaceUnbindRequest
	reports  []WorkspaceActionRunReport
	statuses []int
	code     string
	state    string
}

func newWorkspaceClient(t *testing.T, hub *workspaceHub) *NativeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.paths = append(hub.paths, r.URL.Path)
		status := http.StatusOK
		if len(hub.statuses) > 0 {
			status, hub.statuses = hub.statuses[0], hub.statuses[1:]
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/worker/bind"):
			var request WorkspaceBindRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hub.binds = append(hub.binds, request)
		case strings.HasSuffix(r.URL.Path, "/worker/heartbeat"):
			var request WorkspaceHeartbeatRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hub.beats = append(hub.beats, request)
		case strings.HasSuffix(r.URL.Path, "/worker/unbind"):
			var request WorkspaceUnbindRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hub.unbinds = append(hub.unbinds, request)
		case strings.HasSuffix(r.URL.Path, "/worker/action-runs"):
			var request WorkspaceActionRunReport
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			hub.reports = append(hub.reports, request)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= http.StatusBadRequest {
			code := hub.code
			if code == "" {
				code = "invalid_request"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": "refused"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/worker/action-runs") {
			// The report answers with the run as stored, which is how a first
			// report learns the id every later one has to carry.
			_ = json.NewEncoder(w).Encode(workspacesession.Run{
				ID: "actionrun_1", ActionID: "action_1", WorkspaceID: testWorkspaceID,
				Command: "npm install", Status: workspacesession.RunRunning,
			})
			return
		}
		state := hub.state
		if state == "" {
			state = workspacesession.StateReady
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"owner":     workspacesession.Owner{WorkspaceID: testWorkspaceID, RunnerID: "runner_1", FencingToken: 5},
			"checkout":  WorkspaceCheckout{WorkItemID: "wi_1", Worktree: workspacesession.WorktreeFresh},
			"workspace": workspacesession.Session{ID: testWorkspaceID, State: state},
			"id":        testWorkspaceID,
			"state":     state,
		})
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return native
}

func testIdentity() WorkspaceIdentity {
	return WorkspaceIdentity{LeaseID: tracker.LeaseID("lease_1"), FencingToken: 5}
}

func TestWorkspaceWorkerCallsCarryTheOwnerTuple(t *testing.T) {
	t.Parallel()
	hub := &workspaceHub{}
	client := newWorkspaceClient(t, hub)
	base := "/api/v2/organizations/org_test/projects/prj_test/workspaces/" + testWorkspaceID

	bound, err := client.BindWorkspace(t.Context(), testWorkspaceID, WorkspaceBindRequest{
		WorkspaceIdentity: testIdentity(), Capabilities: workspacesession.Capabilities{Files: true},
		Isolation: workspacesession.IsolationUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Checkout.WorkItemID != "wi_1" || bound.Owner.WorkspaceID != testWorkspaceID {
		t.Fatalf("bind = %+v", bound)
	}
	if _, err := client.HeartbeatWorkspace(t.Context(), testWorkspaceID, WorkspaceHeartbeatRequest{
		WorkspaceIdentity: testIdentity(), State: workspacesession.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.UnbindWorkspace(t.Context(), testWorkspaceID, WorkspaceUnbindRequest{
		WorkspaceIdentity: testIdentity(), Reason: workspacesession.ReasonClosedByActor,
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{base + "/worker/bind", base + "/worker/heartbeat", base + "/worker/unbind"}
	for index, path := range want {
		if hub.paths[index] != path {
			t.Fatalf("call %d hit %q, want %q", index, hub.paths[index], path)
		}
	}
	// The tuple is this workspace's own generation, and every call carries it:
	// a call the hub cannot fence is a call it has to guess about.
	for _, identity := range []WorkspaceIdentity{
		hub.binds[0].WorkspaceIdentity, hub.beats[0].WorkspaceIdentity, hub.unbinds[0].WorkspaceIdentity,
	} {
		if identity.LeaseID != "lease_1" || identity.FencingToken != 5 {
			t.Fatalf("identity = %+v", identity)
		}
	}
}

func TestWorkspaceCallsRefuseAnIdentifierThatIsNotAWorkspace(t *testing.T) {
	t.Parallel()
	hub := &workspaceHub{}
	client := newWorkspaceClient(t, hub)
	// An id the hub never issued must not reach a URL: a client that let one
	// through would be asking the hub to parse what it should have refused.
	for _, id := range []string{"", "ws_short", "../workspaces/other", "conv_" + strings.Repeat("a", 32)} {
		if _, err := client.BindWorkspace(t.Context(), id, WorkspaceBindRequest{WorkspaceIdentity: testIdentity()}); err == nil {
			t.Fatalf("BindWorkspace(%q) was accepted", id)
		}
		if _, err := client.HeartbeatWorkspace(t.Context(), id, WorkspaceHeartbeatRequest{WorkspaceIdentity: testIdentity()}); err == nil {
			t.Fatalf("HeartbeatWorkspace(%q) was accepted", id)
		}
		if err := client.UnbindWorkspace(t.Context(), id, WorkspaceUnbindRequest{WorkspaceIdentity: testIdentity()}); err == nil {
			t.Fatalf("UnbindWorkspace(%q) was accepted", id)
		}
	}
	if len(hub.paths) != 0 {
		t.Fatalf("a refused identifier still reached the hub: %v", hub.paths)
	}
}

func TestWorkspaceErrorsMapOntoTheSentinelsASessionActsOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "stale execution", status: http.StatusConflict, code: "stale_execution", want: ErrStaleWorkspace},
		{name: "stale fencing token", status: http.StatusConflict, code: "stale_fencing_token", want: ErrStaleWorkspace},
		{name: "lease gone", status: http.StatusConflict, code: "lease_not_found", want: ErrStaleWorkspace},
		{name: "workspace gone", status: http.StatusNotFound, code: "not_found", want: ErrNoWorkspace},
		// Everything else is a failure to retry, not a lost workspace: a
		// session that treated a 503 as a lost lease would give up a worktree
		// nobody took away from it.
		{name: "unavailable", status: http.StatusServiceUnavailable, code: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := &workspaceHub{statuses: []int{test.status}, code: test.code}
			client := newWorkspaceClient(t, hub)
			_, err := client.HeartbeatWorkspace(t.Context(), testWorkspaceID, WorkspaceHeartbeatRequest{WorkspaceIdentity: testIdentity()})
			if err == nil {
				t.Fatal("a refused heartbeat must be an error")
			}
			if test.want == nil {
				if errors.Is(err, ErrStaleWorkspace) || errors.Is(err, ErrNoWorkspace) {
					t.Fatalf("%v was read as a lost workspace", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestWorkspaceForWorkItemJoinsAClaimToItsWorkspace(t *testing.T) {
	t.Parallel()
	hub := &workspaceHub{state: workspacesession.StateStarting}
	client := newWorkspaceClient(t, hub)
	session, err := client.WorkspaceForWorkItem(t.Context(), "wi_1", testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != testWorkspaceID || session.State != workspacesession.StateStarting {
		t.Fatalf("session = %+v", session)
	}
	if want := "/api/v2/organizations/org_test/projects/prj_test/work-items/wi_1/workspace"; hub.paths[0] != want {
		t.Fatalf("path = %q, want %q", hub.paths[0], want)
	}
	// A work item id that could change the path is refused before it becomes
	// one, the same rule the workspace id gets.
	for _, item := range []string{"", "wi/../other", "wi?x=1", "wi#1"} {
		if _, err := client.WorkspaceForWorkItem(t.Context(), tracker.NativeWorkItemID(item), testIdentity()); err == nil {
			t.Fatalf("WorkspaceForWorkItem(%q) was accepted", item)
		}
	}
}

func TestRelayURLFollowsTheHubsScheme(t *testing.T) {
	t.Parallel()
	// Only http and https reach here: New refuses any other scheme, so a
	// relay URL is always one of these two.
	tests := []struct {
		name string
		hub  string
		want string
	}{
		{name: "plain", hub: "http://hub.test:7777", want: "ws://hub.test:7777/relay"},
		{name: "tls", hub: "https://hub.test", want: "wss://hub.test/relay"},
		{name: "under a path", hub: "http://hub.test/detent/", want: "ws://hub.test/detent/relay"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, err := New(Config{URL: test.hub, TokenSource: func() string { return "test" }})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			got, err := native.relayURL("/relay")
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("relayURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkspaceClaimerAsksOnlyForWorkspaceItems(t *testing.T) {
	t.Parallel()
	var claims []tracker.NativeClaim
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var claim tracker.NativeClaim
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		claims = append(claims, claim)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tracker.NativeLease{ID: "lease_1", WorkItemID: "wi_1", FencingToken: 5})
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	sessions := 0
	claimer, err := NewWorkspaceClaimer(native, WorkspaceLaneConfig{
		PolicyID: "policy_1", MachineID: "machine_1",
		SessionID: func() (string, error) { sessions++; return "session", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimer.ClaimWorkspace(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("the claimer made %d claims, want one", len(claims))
	}
	if len(claims[0].LabelInclude) != 1 || claims[0].LabelInclude[0] != WorkspaceItemLabel {
		t.Fatalf("label filter = %v, want the workspace label alone", claims[0].LabelInclude)
	}
	// Each claim needs its own session: a session already holding a lease is
	// answered with that lease rather than a new one, which is right for a
	// retrying run and wrong for a lane that wants the next workspace.
	if _, err := claimer.ClaimWorkspace(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 {
		t.Fatalf("the claimer asked for %d sessions across two claims", sessions)
	}
	if claimer.Native() != native {
		t.Fatal("the claimer must expose the client its sessions use")
	}
}

// TestWorkspaceClaimerNamesAReleaseTheHubAlreadyMade is the tail of the
// September 12 fifth dogfood defect: the lane released a lease the hub had
// already let go of and could only see an opaque 409. The hub releases the
// workspace lease itself when a heartbeat ends the workspace, so the lane has
// to be able to tell that answer -- agreement -- from a release that failed.
func TestWorkspaceClaimerNamesAReleaseTheHubAlreadyMade(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "a released or expired lease is a stale workspace", status: http.StatusConflict, code: "stale_fencing_token", want: ErrStaleWorkspace},
		{name: "a lease the hub never had is gone", status: http.StatusNotFound, code: "lease_not_found", want: ErrNoWorkspace},
		{name: "anything else stays what it was", status: http.StatusUnprocessableEntity, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"code":"` + test.code + `","message":"refused"}`))
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			claimer, err := NewWorkspaceClaimer(native, WorkspaceLaneConfig{
				PolicyID: "policy_1", MachineID: "machine_1",
				SessionID: func() (string, error) { return "session", nil }})
			if err != nil {
				t.Fatal(err)
			}
			err = claimer.ReleaseWorkspaceLease(t.Context(), tracker.NativeLease{ID: "lease_1", FencingToken: 5}, "completed")
			if err == nil {
				t.Fatal("the release reported success on a refusal")
			}
			if test.want == nil {
				if errors.Is(err, ErrStaleWorkspace) || errors.Is(err, ErrNoWorkspace) {
					t.Fatalf("release error = %v, want it left as the hub's own refusal", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("release error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestWorkspaceClaimerRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	native := newWorkspaceClient(t, &workspaceHub{})
	session := func() (string, error) { return "session", nil }
	tests := []struct {
		name   string
		client *NativeClient
		config WorkspaceLaneConfig
	}{
		{name: "no client", config: WorkspaceLaneConfig{PolicyID: "p", MachineID: "m", SessionID: session}},
		{name: "no policy", client: native, config: WorkspaceLaneConfig{MachineID: "m", SessionID: session}},
		{name: "no machine", client: native, config: WorkspaceLaneConfig{PolicyID: "p", SessionID: session}},
		{name: "no session source", client: native, config: WorkspaceLaneConfig{PolicyID: "p", MachineID: "m"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewWorkspaceClaimer(test.client, test.config); err == nil {
				t.Fatal("an incomplete claimer must be refused at construction rather than fail on a claim")
			}
		})
	}
}

// The action run report (decisions section 18.12). It is how a
// run-on-worktree-creation run becomes a row: nobody asked for it, so nothing
// else would ever write one.
func TestReportWorkspaceActionRunCarriesTheTupleAndAnswersTheRun(t *testing.T) {
	t.Parallel()
	hub := &workspaceHub{}
	client := newWorkspaceClient(t, hub)

	run, err := client.ReportWorkspaceActionRun(t.Context(), testWorkspaceID, WorkspaceActionRunReport{
		WorkspaceIdentity: testIdentity(), ActionID: "action_1", Status: workspacesession.RunRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The answer carries the id the hub allocated, which is what every later
	// report of the same run has to name.
	if run.ID != "actionrun_1" || run.Status != workspacesession.RunRunning {
		t.Fatalf("run = %+v", run)
	}
	base := "/api/v2/organizations/org_test/projects/prj_test/workspaces/" + testWorkspaceID
	if len(hub.paths) != 1 || hub.paths[0] != base+"/worker/action-runs" {
		t.Fatalf("paths = %v", hub.paths)
	}
	if len(hub.reports) != 1 || hub.reports[0].WorkspaceIdentity != testIdentity() {
		t.Fatalf("report = %+v", hub.reports)
	}
	if hub.reports[0].ActionID != "action_1" || hub.reports[0].Status != workspacesession.RunRunning {
		t.Fatalf("report = %+v", hub.reports[0])
	}

	t.Run("an identifier the hub never issued does not reach a URL", func(t *testing.T) {
		if _, err := client.ReportWorkspaceActionRun(t.Context(), "ws_short", WorkspaceActionRunReport{
			WorkspaceIdentity: testIdentity(), ActionID: "action_1", Status: workspacesession.RunRunning,
		}); err == nil {
			t.Fatal("a malformed workspace id was accepted")
		}
	})
}

// A report the hub fences off has to be distinguishable from a transport
// failure: the first means stop reporting, the second means try again.
func TestReportWorkspaceActionRunMapsAFencedRefusalOntoTheStaleSentinel(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "stale execution", status: http.StatusConflict, code: "stale_execution", want: ErrStaleWorkspace},
		{name: "workspace gone", status: http.StatusNotFound, code: "not_found", want: ErrNoWorkspace},
		{name: "unavailable", status: http.StatusServiceUnavailable, code: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := &workspaceHub{statuses: []int{test.status}, code: test.code}
			client := newWorkspaceClient(t, hub)
			_, err := client.ReportWorkspaceActionRun(t.Context(), testWorkspaceID, WorkspaceActionRunReport{
				WorkspaceIdentity: testIdentity(), ActionID: "action_1", Status: workspacesession.RunRunning,
			})
			if err == nil {
				t.Fatal("a refused report must be an error")
			}
			if test.want == nil {
				if errors.Is(err, ErrStaleWorkspace) || errors.Is(err, ErrNoWorkspace) {
					t.Fatalf("%v was read as a lost workspace", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}
