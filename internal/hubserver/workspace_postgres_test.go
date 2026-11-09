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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
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
	lifetime := 90 * time.Second
	first := newWorkspaceHostedNativeService(t, Config{now: now, Workspace: &WorkspaceConfig{Enabled: true, MaxLifetime: lifetime, IdleTimeout: lifetime, maintenanceTicks: make(chan time.Time), Terminal: WorkspaceTerminalConfig{Enabled: true, Isolation: workspacesession.IsolationSandbox}}})
	config := first.config
	workspace := first.workspaces.config
	workspace.maintenanceTicks = make(chan time.Time)
	config.Workspace = &workspace
	second := openWorkspaceTenantHub(t, config, workspaceTestDB(t, first), "org_first_second")
	hubs := []*Service{first, second}
	type savedWorkspace struct {
		fixture           *relayFixture
		operatorToken     string
		index             int
		action            workspacesession.Action
		run               string
		automatic         workspacesession.Run
		pending           string
		recording         string
		shutdownRun       string
		shutdownStream    string
		shutdownRecording string
		marker            string
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
	automaticStart := func(record *savedWorkspace) workspacesession.Run {
		t.Helper()
		f := record.fixture
		request := workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: record.action.ID, IdempotencyKey: "automatic-start", Status: workspacesession.RunRunning, OutputSequence: 1}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces/"+f.workspace+"/worker/action-runs", f.runner.redemption.Credential, request)
		requireNativeStatus(t, response, http.StatusOK)
		var run workspacesession.Run
		decodeHubResponse(t, response, &run)
		return run
	}
	assertAutomaticReplay := func(record *savedWorkspace) {
		t.Helper()
		replayed := automaticStart(record)
		want := record.automatic
		if replayed.ID != want.ID || replayed.Status != want.Status || replayed.Revision != want.Revision || replayed.OutputBytes != want.OutputBytes || replayed.Command != want.Command {
			t.Fatalf("automatic admission replay=%+v, want committed receipt %+v", replayed, want)
		}
		f := record.fixture
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var listing projectActionRunList
		decodeHubResponse(t, response, &listing)
		wantRuns := 2
		if record.shutdownRun != "" {
			wantRuns++
		}
		if len(listing.Items) != wantRuns {
			t.Fatalf("automatic admission replay left %d runs, want %d", len(listing.Items), wantRuns)
		}
	}
	for tenant, hub := range hubs {
		organization := tracker.OrganizationID(hub.localOrganization)
		project := newNativeFixture(t, hub, organization, "Workspace durability project")
		approveHubTestPolicy(t, hub, project.base+"/policy", hubTestPolicy())
		for index := range 2 {
			f := startRelayFixture(t, &relayFixture{nativeFixture: project, clock: started, terminal: true, exec: true})
			record := &savedWorkspace{fixture: f, operatorToken: newNativeID("token"), index: index, marker: fmt.Sprintf("recorded-%d-%d", tenant, index)}
			userID := newNativeID("user")
			membership, err := hub.config.Hosted.Provider.CreateMembership(t.Context(), userID, hub.config.Hosted.WorkOSOrganizationID, "member")
			if err != nil {
				t.Fatal(err)
			}
			if err := hub.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				credential := accessstore.Credential{OrganizationID: organization, ID: newNativeID("credential"), Name: "Example workspace operator " + f.workspace, Hash: apikey.HashToken(record.operatorToken), Fingerprint: tokenFingerprint(apikey.HashToken(record.operatorToken)), Scope: "operator", NativeOnly: true, Kind: "personal", UserID: membership.UserID, MembershipID: membership.ID, KeyScope: "read", ProjectAccess: "selected", CreatedAt: started, UpdatedAt: started}
				if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: membership.UserID, Email: userID + "@example.test", MembershipID: membership.ID, Role: "member", Active: true, PrincipalID: credential.ID, CreatedAt: started, UpdatedAt: started}); err != nil {
					return err
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: membership.UserID, ProjectID: project.project.ID, CanWrite: false}); err != nil {
					return err
				}
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credential.ID, ProjectID: project.project.ID})
			}); err != nil {
				t.Fatal(err)
			}
			path := f.base + "/workspaces/" + f.workspace
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/bind", "invalid_workspace_credential", nil), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/heartbeat", "invalid_workspace_credential", nil), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/relay-tickets", "invalid_workspace_credential", map[string]any{}), http.StatusUnauthorized)
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
			authored := performHubAPIRequest(t, hub, http.MethodPost, f.base+"/actions", f.token, map[string]any{"idempotency_key": "action-" + f.workspace, "name": fmt.Sprintf("Action %d", index), "command": "printf example", "run_on_worktree_creation": true})
			requireNativeStatus(t, authored, http.StatusCreated)
			decodeHubResponse(t, authored, &record.action)
			record.run = queueRun(record, "run-"+f.workspace)
			for sequence, status := range []string{workspacesession.RunRunning, workspacesession.RunSucceeded} {
				report := workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: record.action.ID, RunID: record.run, Status: status, OutputSequence: int64(sequence + 1), Output: record.marker + "\n"}
				requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/action-runs", f.runner.redemption.Credential, report), http.StatusOK)
			}
			record.automatic = automaticStart(record)
			if record.automatic.ID == "" || record.automatic.Status != workspacesession.RunRunning {
				t.Fatalf("automatic admission=%+v", record.automatic)
			}
			assertAutomaticReplay(record)
			automaticFinished := workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: record.action.ID, RunID: record.automatic.ID, Status: workspacesession.RunSucceeded, OutputSequence: 2, Output: "automatic-" + record.marker + "\n"}
			finished := performHubAPIRequest(t, hub, http.MethodPost, path+"/worker/action-runs", f.runner.redemption.Credential, automaticFinished)
			requireNativeStatus(t, finished, http.StatusOK)
			decodeHubResponse(t, finished, &record.automatic)
			assertAutomaticReplay(record)
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
			shutdownRunner := f.dialRunner(t)
			shutdownPerson := f.dialPerson(t)
			t.Cleanup(func() {
				_ = shutdownPerson.socket.CloseNow()
				_ = shutdownRunner.socket.CloseNow()
			})
			shutdownPerson.send(terminalOpenFrame())
			record.shutdownStream = shutdownRunner.receive().Stream
			shutdownRunner.send(terminalOutputFrame(t, record.shutdownStream, record.marker+"-shutdown"))
			if frame := shutdownPerson.receive(); frame.Type != workspacesession.TypeTerminalOutput || frame.Stream != record.shutdownStream {
				t.Fatalf("shutdown terminal output frame=%+v", frame)
			}
			record.shutdownRun = queueRun(record, "shutdown-"+f.workspace)
			shutdownPerson.send(execRunFrame(record.shutdownRun, record.action))
			forwarded := shutdownRunner.receive()
			if forwarded.Type != workspacesession.TypeExecRun || forwarded.Actor == nil {
				t.Fatalf("shutdown exec frame=%+v", forwarded)
			}
			if run := readRun(record, record.shutdownRun); run.Status != workspacesession.RunRunning || run.StartedAt == nil {
				t.Fatalf("shutdown exec was not admitted: %+v", run)
			}
			shutdownRunner.send(execOutputFrame(forwarded.Stream, workspacesession.ExecOutput{Data: record.marker + "-partial"}))
			if frame := shutdownPerson.receive(); frame.Type != workspacesession.TypeExecOutput || frame.Stream != forwarded.Stream {
				t.Fatalf("shutdown exec output=%+v", frame)
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
				assertAutomaticReplay(record)
				if run := readRun(record, record.shutdownRun); run.Status != workspacesession.RunFailed || run.Reason != workspacesession.RunReasonStreamClosed || run.FinishedAt == nil || run.OutputBytes != int64(len(record.marker+"-partial")) {
					t.Fatalf("in-flight exec after Hub shutdown=%+v", run)
				}
				response := performHubAPIRequest(t, reopened, http.MethodGet, f.base+"/workspaces/"+f.workspace+"/terminal-recordings", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var listing struct {
					Recordings []TerminalRecording `json:"recordings"`
				}
				decodeHubResponse(t, response, &listing)
				if len(listing.Recordings) != 2 {
					t.Fatalf("recordings after live Hub shutdown=%+v", listing.Recordings)
				}
				for _, recording := range listing.Recordings {
					if recording.StreamID == record.shutdownStream {
						if recording.FinishedAt == nil || recording.Artifact == "" || recording.Isolation != workspacesession.IsolationSandbox {
							t.Fatalf("unfinished shutdown recording=%+v", recording)
						}
						record.shutdownRecording = recording.ID
						operatorResponse := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+f.workspace, record.operatorToken, nil)
						requireNativeStatus(t, operatorResponse, http.StatusOK)
						var operatorSession workspacesession.Session
						decodeHubResponse(t, operatorResponse, &operatorSession)
						if len(operatorSession.RelaySessions) != 0 {
							t.Error("operator token exposed owner-only relay audits")
						}
						if err := reopened.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							audits, err := r.(workspaceReader).RecentRelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkspaceID: f.workspace, Limit: 10})
							if err != nil {
								return err
							}
							for _, audit := range audits {
								if audit.ID == recording.RelaySessionID {
									if audit.ClosedAt == nil || audit.CloseReason != "server_shutdown" {
										t.Errorf("relay audit after Hub shutdown=%+v", audit)
									}
									return nil
								}
							}
							t.Errorf("shutdown recording has no durable relay audit: %s", recording.RelaySessionID)
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if record.shutdownRecording == "" {
					t.Fatal("Hub shutdown did not publish its open terminal recording")
				}
			} else if session := readSession(record); session.State != workspacesession.StateReady {
				t.Fatalf("another tenant's restart changed workspace state=%q", session.State)
			}
		}
	}
	clockMu.Lock()
	clock = started.Add(65 * time.Second)
	clockMu.Unlock()
	for _, hub := range hubs {
		hub.workspaces.sweep(t.Context())
		for _, record := range saved {
			f := record.fixture
			if f.service != hub {
				if session := readSession(record); session.State != workspacesession.StateReady {
					t.Fatalf("another tenant's heartbeat recovery changed workspace state=%q", session.State)
				}
				continue
			}
			if session := readSession(record); session.State != workspacesession.StateUnreachable {
				t.Fatalf("missed workspace heartbeats did not become unreachable: %+v", session)
			}
			path := f.base + "/workspaces/" + f.workspace + "/worker/heartbeat"
			request := workspaceHeartbeatRequest{workspaceWorkerIdentity: workspaceIdentity(f.lease), State: workspacesession.StateReady, Capabilities: f.capabilities()}
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path, f.runner.redemption.Credential, request), http.StatusOK)
			if session := readSession(record); session.State != workspacesession.StateReady {
				t.Fatalf("current owner heartbeat did not recover workspace: %+v", session)
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
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/worker/unbind", "invalid_workspace_credential", body), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/worker/unbind", f.runner.redemption.Credential, body), http.StatusNoContent)
		}
	}
	clockMu.Lock()
	clock = started.Add(lifetime)
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
		credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
		for _, id := range []string{record.run, record.automatic.ID, record.shutdownRun} {
			row, err := f.service.readActionRunRecordByID(t.Context(), scope, id)
			if err != nil || row.ID != id || row.ActionID != record.action.ID {
				t.Fatalf("scoped action-run lookup=%+v error=%v", row, err)
			}
			metadata, err := f.service.hydrateActionRunRecord(t.Context(), row, false)
			if err != nil || metadata.Command != "printf example" || metadata.Output != "" || metadata.OutputBytes == 0 {
				t.Fatalf("scoped action-run metadata=%+v error=%v", metadata, err)
			}
		}
		foreign := saved[(index+2)%len(saved)].automatic.ID
		if _, err := f.service.readActionRunRecordByID(t.Context(), scope, foreign); !isNativeNotFound(err) {
			t.Fatalf("foreign action-run lookup error=%v, want opaque not found", err)
		}
		if run := readRun(record, record.run); run.Status != workspacesession.RunSucceeded || run.Command != "printf example" || run.OutputBytes != int64(len(record.marker+"\n")) {
			t.Fatalf("completed action after reopen=%+v", run)
		}
		if run := readRun(record, record.automatic.ID); run.Status != workspacesession.RunSucceeded || run.Revision != record.automatic.Revision || run.OutputBytes != int64(len("automatic-"+record.marker+"\n")) {
			t.Fatalf("automatic action after reopen=%+v", run)
		}
		if run := readRun(record, record.pending); run.Status != workspacesession.RunFailed || run.Reason != workspacesession.RunReasonWorkspaceEnd || run.StartedAt == nil || run.FinishedAt == nil || run.FinishedAt.Before(*run.StartedAt) {
			t.Fatalf("orphaned action after reopen=%+v", run)
		}
		output := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs/"+record.run+"/output", f.token, nil)
		requireNativeStatus(t, output, http.StatusOK)
		if output.Body.String() != record.marker+"\n" {
			t.Fatalf("action output after reopen=%q", output.Body.String())
		}
		automaticOutput := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs/"+record.automatic.ID+"/output", f.token, nil)
		requireNativeStatus(t, automaticOutput, http.StatusOK)
		if automaticOutput.Body.String() != "automatic-"+record.marker+"\n" {
			t.Fatalf("automatic action output after reopen=%q", automaticOutput.Body.String())
		}
		cast := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+f.workspace+"/terminal-recordings/"+record.recording, f.token, nil)
		requireNativeStatus(t, cast, http.StatusOK)
		if !strings.Contains(cast.Body.String(), record.marker) || !strings.Contains(cast.Header().Get("Content-Type"), "asciicast") {
			t.Fatalf("terminal recording after reopen=%q", cast.Body.String())
		}
		if run := readRun(record, record.shutdownRun); run.Status != workspacesession.RunFailed || run.Reason != workspacesession.RunReasonStreamClosed || run.OutputBytes != int64(len(record.marker+"-partial")) {
			t.Fatalf("shutdown exec after final reopen=%+v", run)
		}
		shutdownOutput := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+record.action.ID+"/runs/"+record.shutdownRun+"/output", f.token, nil)
		requireNativeStatus(t, shutdownOutput, http.StatusOK)
		if shutdownOutput.Body.String() != record.marker+"-partial" {
			t.Fatalf("shutdown exec output after final reopen=%q", shutdownOutput.Body.String())
		}
		shutdownCast := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+f.workspace+"/terminal-recordings/"+record.shutdownRecording, f.token, nil)
		requireNativeStatus(t, shutdownCast, http.StatusOK)
		if !strings.Contains(shutdownCast.Body.String(), record.marker+"-shutdown") {
			t.Fatalf("shutdown recording after final reopen=%q", shutdownCast.Body.String())
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(workspaceReader).WorkspaceOccupancy(t.Context(), f.project.ID, f.workspace, 10)
			if err != nil {
				return err
			}
			wantEnd := started.Add(lifetime)
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
