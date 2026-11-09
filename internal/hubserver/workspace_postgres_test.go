package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestPostgresWorkspaceTenantLifecycleSurvivesHubRestart(t *testing.T) {
	started := time.Now().UTC().Truncate(time.Microsecond)
	clock := started
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	first := newWorkspaceNativeService(t, Config{now: now, Workspace: &WorkspaceConfig{Enabled: true, MaxLifetime: time.Minute, IdleTimeout: time.Minute, maintenanceTicks: make(chan time.Time), Terminal: WorkspaceTerminalConfig{Enabled: true, Isolation: workspacesession.IsolationSandbox}}})
	config := first.config
	workspace := first.workspaces.config
	workspace.maintenanceTicks = make(chan time.Time)
	config.Workspace = &workspace
	second := openWorkspaceTenantHub(t, config, workspaceTestDB(t, first), "org_first_second")
	hubs := []*Service{first, second}
	type savedWorkspace struct {
		fixture   *relayFixture
		index     int
		action    workspacesession.Action
		run       string
		pending   string
		recording string
		marker    string
	}
	var saved []*savedWorkspace
	readSession := func(record *savedWorkspace) workspacesession.Session {
		t.Helper()
		f := record.fixture
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+f.workspace, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var session workspacesession.Session
		decodeHubResponse(t, response, &session)
		return session
	}
	readRun := func(record *savedWorkspace, id string) workspacesession.Run {
		t.Helper()
		f := record.fixture
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs/"+id, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var run workspacesession.Run
		decodeHubResponse(t, response, &run)
		return run
	}
	queueRun := func(record *savedWorkspace, key string) string {
		t.Helper()
		f := record.fixture
		body := projectActionRunRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, WorkspaceID: f.workspace, ExpectedActionRevision: tracker.Revision(record.action.Revision)}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions/"+record.action.ID+"/runs", f.token, body)
		requireNativeStatus(t, response, http.StatusAccepted)
		var receipt projectActionRunReceipt
		decodeHubResponse(t, response, &receipt)
		return receipt.RunID
	}
	for tenant, hub := range hubs {
		organization := tracker.OrganizationID(hub.localOrganization)
		project := newNativeFixture(t, hub, organization, "Workspace durability project")
		approveHubTestPolicy(t, hub, project.base+"/policy", hubTestPolicy())
		for index := range 2 {
			f := startRelayFixture(t, &relayFixture{nativeFixture: project, clock: started, terminal: true, exec: true})
			record := &savedWorkspace{fixture: f, index: index, marker: fmt.Sprintf("recorded-%d-%d", tenant, index)}
			path := f.base + "/workspaces/" + f.workspace
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/bind", "", nil), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/heartbeat", "", nil), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/relay-tickets", "", map[string]any{}), http.StatusUnauthorized)
			runner := f.dialRunner(t)
			ticket := f.ticket(t)
			url := f.socketURL(path + "/relay?ticket=" + ticket)
			header := http.Header{"Authorization": {"Bearer " + f.token}}
			before := f.attachedConnections()
			person := f.dial(t, url, header)
			f.awaitAttached(t, before)
			refused, dialResponse, err := websocket.Dial(testenv.WaitContext(t, t.Context()), url, &websocket.DialOptions{HTTPHeader: header})
			closeRelayDial(dialResponse)
			if err == nil {
				_ = refused.CloseNow()
				t.Fatal("a redeemed ticket opened another relay")
			}
			if dialResponse == nil || dialResponse.StatusCode != http.StatusNotFound {
				t.Fatal("redeemed ticket did not return opaque refusal")
			}
			person.send(terminalOpenFrame())
			stream := runner.receive().Stream
			runner.send(terminalOutputFrame(t, stream, record.marker))
			if frame := person.receive(); frame.Type != workspacesession.TypeTerminalOutput || frame.Stream != stream {
				t.Fatalf("terminal output frame=%+v", frame)
			}
			person.send(workspacesession.Frame{Channel: workspacesession.ChannelTerminal, Stream: stream, Type: workspacesession.TypeClose})
			person.receive()
			response := performHubAPIRequest(t, hub, http.MethodGet, path+"/terminal-recordings", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var listing struct {
				Recordings []TerminalRecording `json:"recordings"`
			}
			decodeHubResponse(t, response, &listing)
			if len(listing.Recordings) != 1 || listing.Recordings[0].Isolation != workspacesession.IsolationSandbox || listing.Recordings[0].Artifact == "" {
				t.Fatalf("terminal recording=%+v", listing.Recordings)
			}
			record.recording = listing.Recordings[0].ID
			_ = person.socket.CloseNow()
			_ = runner.socket.CloseNow()
			testenv.WaitUntil(t, t.Context(), func() error {
				relay := hub.workspaces.relay
				relay.mu.Lock()
				defer relay.mu.Unlock()
				room := relay.rooms[f.workspace]
				if room != nil && (room.runner != nil || len(room.people) != 0) {
					return errors.New("relay connections are still attached")
				}
				return nil
			})
			authored := performHubAPIRequest(t, hub, http.MethodPost, f.base+"/actions", f.token, map[string]any{"idempotency_key": "action-" + f.workspace, "name": fmt.Sprintf("Action %d", index), "command": "printf example"})
			requireNativeStatus(t, authored, http.StatusCreated)
			decodeHubResponse(t, authored, &record.action)
			record.run = queueRun(record, "run-"+f.workspace)
			for sequence, status := range []string{workspacesession.RunRunning, workspacesession.RunSucceeded} {
				report := workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: record.action.ID, RunID: record.run, Status: status, OutputSequence: int64(sequence + 1), Output: record.marker + "\n"}
				requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/action-runs", f.runner.redemption.Credential, report), http.StatusOK)
			}
			lookup := httptest.NewRequest(http.MethodGet, f.base+"/work-items/"+f.item+"/workspace", nil).WithContext(t.Context())
			lookup.Header.Set("Authorization", "Bearer "+f.runner.redemption.Credential)
			lookup.Header.Set("X-Detent-Lease", string(f.lease.ID))
			lookup.Header.Set("X-Detent-Fencing-Token", fmt.Sprint(f.lease.FencingToken))
			found := httptest.NewRecorder()
			hub.echo.ServeHTTP(found, lookup)
			requireNativeStatus(t, found, http.StatusOK)
			var session workspacesession.Session
			decodeHubResponse(t, found, &session)
			if session.ID != f.workspace {
				t.Fatalf("dispatch lookup returned workspace %q", session.ID)
			}
			saved = append(saved, record)
		}
	}
	for tenant, hub := range hubs {
		clockMu.Lock()
		clock = clock.Add(time.Second)
		clockMu.Unlock()
		reopened := reopenWorkspaceHub(t, hub)
		hubs[tenant] = reopened
		for _, record := range saved {
			f := record.fixture
			if f.project.OrganizationID == tracker.OrganizationID(reopened.localOrganization) {
				f.service = reopened
				session := readSession(record)
				owner := f.record(t)
				if session.State != workspacesession.StateRequested || session.RunnerID != f.runner.binding.RunnerID || session.MachineID != string(f.runner.binding.MachineID) || owner.RebindDeadline == nil || owner.LeaseID != f.lease.ID || owner.FencingToken != f.lease.FencingToken {
					t.Fatalf("workspace restart lost its owner: %+v / %+v", session, owner.owner())
				}
				f.bind(t)
			} else if session := readSession(record); session.State != workspacesession.StateReady {
				t.Fatalf("another tenant's restart changed workspace state=%q", session.State)
			}
		}
	}
	unboundAt := now()
	for index, record := range saved {
		f := record.fixture
		record.pending = queueRun(record, "pending-"+f.workspace)
		path := f.base + "/workspaces/" + f.workspace
		foreign := saved[(index+2)%len(saved)].fixture.token
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/terminal-recordings/"+record.recording, foreign, nil), http.StatusUnauthorized)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/terminal-recordings/"+record.recording, "", nil), http.StatusUnauthorized)
		if record.index == 0 {
			body := workspaceUnbindRequest{workspaceWorkerIdentity: workspaceIdentity(f.lease), Reason: workspacesession.ReasonClosedByActor}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/worker/unbind", "", body), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/worker/unbind", f.runner.redemption.Credential, body), http.StatusNoContent)
		}
	}
	clockMu.Lock()
	clock = started.Add(time.Minute)
	clockMu.Unlock()
	swept := map[tracker.OrganizationID]bool{}
	for _, hub := range hubs {
		hub.workspaces.sweep(t.Context())
		swept[tracker.OrganizationID(hub.localOrganization)] = true
		for _, record := range saved {
			wantState, wantRun := workspacesession.StateReady, workspacesession.RunQueued
			if record.index == 0 {
				wantState = workspacesession.StateClosed
			}
			if swept[record.fixture.project.OrganizationID] {
				wantState, wantRun = workspacesession.StateClosed, workspacesession.RunFailed
			}
			if session := readSession(record); session.State != wantState {
				t.Fatalf("tenant sweep workspace state=%q want %q", session.State, wantState)
			}
			if run := readRun(record, record.pending); run.Status != wantRun {
				t.Fatalf("tenant sweep run status=%q want %q", run.Status, wantRun)
			}
		}
	}
	for tenant, hub := range hubs {
		hubs[tenant] = reopenWorkspaceHub(t, hub)
	}
	for index, record := range saved {
		f := record.fixture
		f.service = hubs[index/2]
		if session := readSession(record); session.State != workspacesession.StateClosed {
			t.Fatalf("ended workspace after reopen=%+v", session)
		}
		if run := readRun(record, record.run); run.Status != workspacesession.RunSucceeded || run.Command != "printf example" || run.OutputBytes != int64(len(record.marker+"\n")) {
			t.Fatalf("completed action after reopen=%+v", run)
		}
		if run := readRun(record, record.pending); run.Status != workspacesession.RunFailed || run.Reason != workspacesession.RunReasonWorkspaceEnd || run.StartedAt == nil || run.FinishedAt == nil || run.FinishedAt.Before(*run.StartedAt) {
			t.Fatalf("orphaned action after reopen=%+v", run)
		}
		output := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs/"+record.run+"/output", f.token, nil)
		requireNativeStatus(t, output, http.StatusOK)
		if output.Body.String() != record.marker+"\n" {
			t.Fatalf("action output after reopen=%q", output.Body.String())
		}
		cast := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+f.workspace+"/terminal-recordings/"+record.recording, f.token, nil)
		requireNativeStatus(t, cast, http.StatusOK)
		if !strings.Contains(cast.Body.String(), record.marker) || !strings.Contains(cast.Header().Get("Content-Type"), "asciicast") {
			t.Fatalf("terminal recording after reopen=%q", cast.Body.String())
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(workspaceReader).WorkspaceOccupancy(t.Context(), f.project.ID, f.workspace, 10)
			if err != nil {
				return err
			}
			wantEnd := started.Add(time.Minute)
			if record.index == 0 {
				wantEnd = unboundAt
			}
			if len(rows) != 1 || rows[0].EndedAt == nil || !rows[0].EndedAt.Equal(wantEnd) {
				t.Fatalf("workspace occupancy=%+v want one interval ending %s", rows, wantEnd)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
