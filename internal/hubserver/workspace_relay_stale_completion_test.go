package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type actionRunCompletionReadKey struct{}

type actionRunCompletionReadBarrier struct {
	*postgres.Repository
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *actionRunCompletionReadBarrier) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if err := r.Repository.ReadTenant(ctx, organization, workload, read); err != nil {
		return err
	}
	if ctx.Value(actionRunCompletionReadKey{}) != r {
		return nil
	}
	r.once.Do(func() { close(r.reached) })
	select {
	case <-r.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestPostgresWorkspaceCompletionUsesLockedRunState(t *testing.T) {
	f := newRelayFixture(t, withExec)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.runner.redemption.Credential), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	assertOutputOwner := func(t *testing.T, run string) {
		t.Helper()
		var archive string
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
			row, err := reader.(workspaceReader).ActionRunByID(t.Context(), run)
			if err != nil {
				return err
			}
			if row.OutputObjectKey == nil {
				return fmt.Errorf("run has no output reference")
			}
			object, err := reader.ArchiveObjectByReference(t.Context(), workspaceArchive(*row.OutputObjectKey, row.OutputSha256, row.OutputBytes, row.OutputContentBytes))
			archive = object.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var owners, current int
		if err := workspaceTestDB(t, f.service).QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE archive_id=$3) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='project-action-output' AND owner_id=$2`, f.project.OrganizationID, actionstore.ArchiveOwnerID(f.project.ID, run), archive).Scan(&owners, &current); err != nil {
			t.Fatal(err)
		}
		if owners != 1 || current != 1 {
			t.Errorf("output owners=%d current=%d, want one current owner", owners, current)
		}
	}
	var saved []actionRunRecord
	for _, test := range []struct {
		name        string
		closeStream bool
		capture     bool
		terminal    bool
		wantStatus  string
		wantReason  string
		wantOutput  string
	}{
		{name: "relay exit", capture: true, wantStatus: workspacesession.RunSucceeded, wantOutput: "captured final output"},
		{name: "stream close", closeStream: true, capture: true, wantStatus: workspacesession.RunFailed, wantReason: workspacesession.RunReasonStreamClosed, wantOutput: "captured final output"},
		{name: "stored output fallback", closeStream: true, wantStatus: workspacesession.RunFailed, wantReason: workspacesession.RunReasonStreamClosed, wantOutput: "new worker output"},
		{name: "terminal replay", capture: true, terminal: true, wantStatus: workspacesession.RunSucceeded, wantOutput: "new worker output"},
	} {
		t.Run(test.name, func(t *testing.T) {
			action := f.action(t, fmt.Sprintf("Example %s", test.name), "printf example")
			run := f.queuedRun(t, action)
			report := workspaceActionRunReport{WorkspaceIdentity: workspaceIdentity(f.lease), ActionID: action.ID, RunID: run, Status: workspacesession.RunRunning, Output: "initial worker output"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces/"+f.workspace+"/worker/action-runs", f.runner.redemption.Credential, report), http.StatusOK)
			before := f.runRow(t, run)
			barrier := &actionRunCompletionReadBarrier{Repository: f.service.database.postgres, reached: make(chan struct{}), resume: make(chan struct{})}
			config := f.service.config
			config.TenantRepository = barrier
			server := &Service{config: config}
			service := &workspaceService{server: server, config: f.service.workspaces.config, logger: f.service.workspaces.logger}
			relay := newWorkspaceRelay(service)
			service.relay = relay
			record := f.record(t)
			connection := &relayConnection{id: "relayrunner_example", workspaceID: f.workspace, runner: true, scope: scope, owner: record.owner()}
			stream := &relayStream{id: "relayhub:1", channel: workspacesession.ChannelExec, connectionID: hubStreamOwner, hubOwned: true}
			state := &execRunState{organization: f.project.OrganizationID, runID: run, actionID: action.ID}
			if test.capture {
				state.output = []byte("captured final output")
			}
			room := relay.ensureRoom(f.workspace)
			room.runner = connection
			room.streams[stream.id] = stream
			room.execRuns[stream.id] = state
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), actionRunCompletionReadKey{}, barrier))
			finished := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(barrier.resume) }) }
			t.Cleanup(func() {
				release()
				cancel()
				<-finished
			})
			go func() {
				defer close(finished)
				if test.closeStream {
					relay.closeStream(ctx, f.workspace, stream.id)
				} else {
					service.recordExecFrame(ctx, connection, stream, execExitedFrame(stream.id, 0))
				}
			}()
			select {
			case <-barrier.reached:
			case <-finished:
				t.Fatal("completion finished before its read barrier")
			}
			report.Output = "new worker output"
			code := 0
			if test.terminal {
				report.Status = workspacesession.RunSucceeded
				report.ExitCode = &code
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces/"+f.workspace+"/worker/action-runs", f.runner.redemption.Credential, report), http.StatusOK)
			reported := f.runRow(t, run)
			if reported.Revision != before.Revision+1 || reported.Output != report.Output {
				t.Fatalf("concurrent report did not commit: before=%+v after=%+v", before, reported)
			}
			release()
			<-finished
			completed := f.runRow(t, run)
			if completed.Status != test.wantStatus || completed.Reason != test.wantReason || completed.Output != test.wantOutput || completed.FinishedAt == nil {
				t.Errorf("completion used stale state: %+v", completed)
			}
			if test.closeStream && completed.ExitCode != nil || !test.closeStream && (completed.ExitCode == nil || *completed.ExitCode != 0) {
				t.Errorf("completion exit code=%v, close_stream=%v", completed.ExitCode, test.closeStream)
			}
			if test.terminal && completed.Revision != reported.Revision {
				t.Errorf("terminal replay mutated revision: got=%d want=%d", completed.Revision, reported.Revision)
			}
			if test.terminal && (completed.FinishedAt == nil || reported.FinishedAt == nil || !completed.FinishedAt.Equal(*reported.FinishedAt)) {
				t.Errorf("terminal replay changed finish time: got=%v want=%v", completed.FinishedAt, reported.FinishedAt)
			}
			relay.mu.Lock()
			_, retained := room.execRuns[stream.id]
			relay.mu.Unlock()
			if retained {
				t.Error("completed run retained in-flight execution state")
			}
			assertOutputOwner(t, run)
			saved = append(saved, completed)
		})
	}
	f.service = reopenWorkspaceHub(t, f.service)
	for _, expected := range saved {
		actual := f.runRow(t, expected.ID)
		exitMatches := actual.ExitCode == nil && expected.ExitCode == nil || actual.ExitCode != nil && expected.ExitCode != nil && *actual.ExitCode == *expected.ExitCode
		if actual.Status != expected.Status || actual.Reason != expected.Reason || actual.Output != expected.Output || actual.Revision != expected.Revision || actual.FinishedAt == nil || !exitMatches {
			t.Errorf("completion changed after full reopen: before=%+v after=%+v", expected, actual)
		}
		assertOutputOwner(t, expected.ID)
	}
}
