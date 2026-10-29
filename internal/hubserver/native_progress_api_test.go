package hubserver

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeProgressRequestReplayAndFencing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		changed       bool
		released      bool
		editedOlder   bool
		fullContext   bool
		active        bool
		success       bool
		noProduct     bool
		blocked       bool
		staleRevision bool
	}{
		{name: "decision comment does not create new evidence"},
		{name: "succeeded without change is checked", success: true},
		{name: "succeeded without work product is checked", success: true, noProduct: true},
		{name: "succeeded with source recovery blocker is checked", success: true, blocked: true},
		{name: "stale item revision rejects publication", staleRevision: true},
		{name: "full context remains stable after publication", fullContext: true},
		{name: "admitted active worker still reads prior failures", active: true},
		{name: "new human input rejects stale decision", changed: true},
		{name: "edited older human input rejects stale decision", changed: true, editedOlder: true},
		{name: "released lease cannot publish decision", released: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed",
				TerminalFailure: &tracker.NativeTerminalFailure{Error: "422: landing requires security audit evidence for its exact base and head", ErrorClass: "publication", Stage: "publication", ObservedAt: f.now},
			}}
			if test.success {
				event.Data.Outcome = "succeeded"
				event.Data.TerminalFailure = nil
				if !test.noProduct {
					event.Data.Finalization = &tracker.NativeFinalization{Changed: false, Settled: true, ObservedAt: f.now}
				}
				if test.blocked {
					event.Data.Finalization.Changed = true
					event.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", ReasonCode: "source_recovery_required"}
				}
			}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			if test.success {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", newNativeID("checkpoint"), ""
				checkpoint.Data.Finalization, checkpoint.Data.Disposition = nil, nil
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, checkpoint), http.StatusOK)
				event.Data.Sequence++
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: "conversation-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.lease)
			params := url.Values{"policy": {f.policy}, "instance": {string(f.lease.MachineID)}}
			read := func() nativeProgressView {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/progress?"+params.Encode(), f.worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view nativeProgressView
				decodeHubResponse(t, response, &view)
				return view
			}
			if test.active {
				started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: f.policy}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, started), http.StatusOK)
			}
			if test.fullContext {
				for i := range progresscheck.MaxComments {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("full-%d", i)}, Body: "Recorded evidence"}), http.StatusOK)
				}
			}
			var older tracker.NativeComment
			if test.editedOlder {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "older-input"}, Body: "Original human input"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &older)
				for i := range progresscheck.MaxComments {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("context-%d", i)}, Body: "Later context"}), http.StatusOK)
				}
			}
			view := read()
			if len(view.Request.Attempts) != 1 || view.Request.Attempts[0].ID != f.attempt || (!test.success && view.Request.Attempts[0].FailureClass != "publication") {
				t.Fatalf("request attempts=%+v", view.Request.Attempts)
			}
			id, err := view.Request.ID()
			if err != nil {
				t.Fatal(err)
			}
			input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, RequestID: id, PolicyID: f.policy, Instance: string(f.lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Reason: "Produce audit evidence on the current source", Evidence: []string{f.attempt}}}
			currentResponse := performHubAPIRequest(t, f.service, http.MethodGet, path, f.worker, nil)
			requireNativeStatus(t, currentResponse, http.StatusOK)
			var current tracker.NativeIssue
			decodeHubResponse(t, currentResponse, &current)
			input.ExpectedRevision = current.Revision
			if test.staleRevision {
				input.ExpectedRevision++
			}
			if test.changed {
				if test.editedOlder {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+older.ID, testHubAdminToken, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edited-input"}, ExpectedRevision: older.Revision, Body: "The audit protocol changed"}), http.StatusOK)
				} else {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "human-input"}, Body: "The audit protocol changed"}), http.StatusOK)
				}
			}
			if test.released {
				f.release(t)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", f.worker, input)
			if test.changed || test.released || test.staleRevision {
				if response.Code == http.StatusOK {
					t.Fatal("stale evidence or released authority published decision")
				}
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_progress_decisions WHERE work_item_id=?", f.issue.WorkItemID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("rejected decision persisted")
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", f.worker, input), http.StatusOK)
			replay := read()
			replayID, err := replay.Request.ID()
			if err != nil || replayID != id || replay.Record == nil {
				t.Fatalf("decision self-invalidated: id=%s want=%s record=%+v err=%v", replayID, id, replay.Record, err)
			}
			var comments int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", f.issue.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			wantComments := 1
			if test.fullContext {
				wantComments += progresscheck.MaxComments
			}
			if comments != wantComments {
				t.Fatalf("replay created %d comments", comments)
			}
		})
	}
}
