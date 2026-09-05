package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func claimNativeAttempt(t *testing.T, f nativeFixture, worker, machine, session string, item tracker.NativeWorkItemID) tracker.NativeLease {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": machine, "hostname": machine, "version": "test", "capacity": 1}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
		PolicyID: hubTestPolicy().ID, WorkItemID: item, MachineID: tracker.MachineID(machine), SessionID: session,
		TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
	})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	return lease
}

func nativeStartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: newNativeID("run"), AttemptID: newNativeID("attempt")}}
}

func nativeTestCheckpoint() *tracker.NativeCheckpoint {
	return &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
}

func TestNativeOrderedAttemptLifecycle(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "ordered")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	for _, test := range []struct {
		name   string
		event  tracker.NativeRunEvent
		status int
	}{
		{"checkpoint before start", checkpoint, http.StatusConflict},
		{"start", start, http.StatusOK},
		{"same command", start, http.StatusOK},
		{"completion skips checkpoint", finish, http.StatusConflict},
		{"checkpoint", checkpoint, http.StatusOK},
		{"complete", finish, http.StatusOK},
		{"duplicate completion", finish, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, test.event), test.status)
		})
	}
	for _, event := range []tracker.NativeRunEvent{start, checkpoint, finish} {
		event.IdempotencyKey += "-new-key"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	}
	for _, test := range []struct {
		name string
		edit func(*tracker.NativeRunEvent)
	}{
		{"different terminal result", func(e *tracker.NativeRunEvent) { e.Data.Outcome = "failed" }},
		{"late progress", func(e *tracker.NativeRunEvent) {
			e.Type = "run.checkpointed"
			e.Data.Sequence = 4
			e.Data.Outcome = ""
			e.Data.Handoff = nativeTestCheckpoint()
		}},
		{"second attempt on same lease", func(e *tracker.NativeRunEvent) { *e = nativeStartedEvent(lease) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := finish
			test.edit(&event)
			event.IdempotencyKey = test.name
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusConflict)
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var attempts tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, response, &attempts)
	if len(attempts.Items) != 1 || attempts.Items[0].Sequence != 3 || attempts.Items[0].Status != "succeeded" || attempts.Items[0].Checkpoint == nil || attempts.Items[0].Checkpoint.WorktreeState != "dirty" || attempts.Items[0].MachineID != lease.MachineID || attempts.Items[0].SessionID != lease.SessionID {
		t.Fatalf("attempt projection = %#v", attempts)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type LIKE 'run.%'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("run event count = %d, error = %v", count, err)
	}
}

func TestNativeRecoveryAfterReassignmentAndRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	f := newNativeFixture(t, openTestService(t, config), "", "recovery")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	other := f.worker(t, "replacement")
	lease := claimNativeAttempt(t, f, worker, "first-machine", "first-session", issue.WorkItemID)
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "push-ambiguity", 2
	event.Data.Handoff = nativeTestCheckpoint()
	event.Data.Handoff.ExternalEffect, event.Data.Handoff.EffectState, event.Data.Handoff.EffectID = "git_push", "ambiguous", newNativeID("effect")
	event.Data.Handoff.HeadSHA = strings.Repeat("a", 40)
	event.Data.Handoff.ExpectedHeadSHA = strings.Repeat("b", 40)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, config)
	now = now.Add(90*time.Second + time.Nanosecond)
	replacement := claimNativeAttempt(t, f, other, "other-machine", "other-session", issue.WorkItemID)
	if replacement.FencingToken <= lease.FencingToken {
		t.Fatal("lease reassignment did not advance fencing")
	}
	for _, test := range []struct {
		name     string
		mutation tracker.Mutation
	}{
		{"omitted fence", tracker.Mutation{IdempotencyKey: "omitted-fence"}},
		{"expired owner", tracker.Mutation{IdempotencyKey: "expired-owner", LeaseID: lease.ID, FencingToken: lease.FencingToken}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", worker, tracker.CreateComment{Mutation: test.mutation, Body: "stale mutation"}), http.StatusConflict)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", other, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "new-owner", LeaseID: replacement.ID, FencingToken: replacement.FencingToken}, Body: "current owner"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "operator"}, Body: "operator collaboration"}), http.StatusOK)
	for _, operation := range []string{"renew", "release", "events"} {
		t.Run("stale "+operation, func(t *testing.T) {
			url := f.base + "/leases/" + string(lease.ID) + "/" + operation
			var request any = tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90, Reason: "completed"}
			if operation == "events" {
				url = path + "/events"
				event.IdempotencyKey, event.Type, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "late-finish", "run.finished", 3, "succeeded", nil
				request = event
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, url, worker, request), http.StatusConflict)
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", other, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 1 || page.Items[0].Status != "interrupted" || page.Items[0].Checkpoint == nil || page.Items[0].Checkpoint.EffectState != "ambiguous" || page.Items[0].Checkpoint.WorktreeState != "dirty" {
		t.Fatalf("recovery lost evidence: %#v", page)
	}
	next := nativeStartedEvent(replacement)
	next.Data.RunID = event.Data.RunID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", other, next), http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", other, nil)
	decodeHubResponse(t, response, &page)
	if page.NextCursor == "" {
		t.Fatal("attempt history was not paginated")
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1&cursor="+page.NextCursor, other, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 1 || page.Items[0].FencingToken != replacement.FencingToken || page.Items[0].Status != "running" {
		t.Fatalf("successor page = %#v", page)
	}
}

func TestNativeConcurrentOrderedEvents(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "concurrent")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	event := nativeStartedEvent(claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID))
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range 8 {
		group.Go(func() {
			<-start
			request := event
			request.IdempotencyKey = fmt.Sprintf("concurrent-%d", index)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, request), http.StatusOK)
		})
	}
	close(start)
	group.Wait()
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempt_events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate progress: %d, %v", count, err)
	}
}

func TestNativeCheckpointValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*tracker.NativeRunData)
	}{
		{"negative sequence", func(d *tracker.NativeRunData) { d.Sequence = -1 }},
		{"raw identity", func(d *tracker.NativeRunData) { d.Identity.Model = "raw prompt text" }},
		{"missing handoff", func(d *tracker.NativeRunData) { d.Handoff = nil }},
		{"unknown availability", func(d *tracker.NativeRunData) { d.Handoff.Availability = "probably" }},
		{"raw head", func(d *tracker.NativeRunData) { d.Handoff.HeadSHA = "/local/path" }},
		{"unbound effect", func(d *tracker.NativeRunData) { d.Handoff.ExternalEffect = "git_push" }},
		{"customer availability assertion", func(d *tracker.NativeRunData) {
			d.Handoff.Storage = "customer_store"
			d.ArtifactIDs = []string{newNativeID("artifact")}
		}},
		{"untyped change", func(d *tracker.NativeRunData) {
			d.Handoff.Change = &tracker.NativeChangeReference{ChangeID: "https://example.com"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}, Handoff: nativeTestCheckpoint()}
			test.edit(&data)
			if err := validateNativeExecution(data, "run.checkpointed"); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
}
