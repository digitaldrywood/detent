package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func claimNativeAttempt(t *testing.T, f nativeFixture, worker, machine, session string, item tracker.NativeWorkItemID, policyIDs ...string) tracker.NativeLease {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": machine, "hostname": machine, "version": "test", "capacity": 1}), http.StatusOK)
	policyID := hubTestPolicy().ID
	if len(policyIDs) > 0 {
		policyID = policyIDs[0]
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
		PolicyID: policyID, WorkItemID: item, MachineID: tracker.MachineID(machine), SessionID: session,
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
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	forged := start
	forged.IdempotencyKey = "forged-evidence"
	forged.Data.Evidence = []tracker.NativeEvidence{{AttachmentID: "att_forged", Caption: "Not uploaded", Reference: "![forged](attachment:att_forged)"}}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	lifecycleTest := t
	for _, test := range []struct {
		name    string
		event   tracker.NativeRunEvent
		status  int
		restart bool
	}{
		{"evidence without upload", forged, http.StatusUnprocessableEntity, false},
		{"checkpoint before start", checkpoint, http.StatusConflict, false},
		{"start", start, http.StatusOK, false},
		{"same command", start, http.StatusOK, false},
		{"completion skips checkpoint", finish, http.StatusConflict, false},
		{"checkpoint", checkpoint, http.StatusOK, false},
		{"complete after restart", finish, http.StatusOK, true},
		{"duplicate completion", finish, http.StatusOK, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.restart {
				config := f.service.config
				started := time.Now()
				shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := f.service.Shutdown(shutdownContext); err != nil {
					t.Fatal(err)
				}
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(lifecycleTest, config)
				t.Logf("hub_restart_seconds=%.6f", time.Since(started).Seconds())
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90})
				requireNativeStatus(t, response, http.StatusOK)
				var renewed tracker.NativeLease
				decodeHubResponse(t, response, &renewed)
				if renewed.ID != lease.ID || renewed.FencingToken != lease.FencingToken || renewed.SessionID != lease.SessionID {
					t.Fatalf("restart changed execution ownership: %+v", renewed)
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: lease.PolicyID, WorkItemID: issue.WorkItemID, MachineID: lease.MachineID, SessionID: "replacement", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusConflict)
			}
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
	event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 27, Phase: "implementation", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: now}}, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, Dropped: 3, Unpaired: 2, Coverage: "partial", AsOf: now}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	for _, elapsed := range []time.Duration{30 * time.Second, 70 * time.Second} {
		now = now.Add(elapsed)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
	}
	var evidence tracker.NativeRuntimeEvidence
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime", worker, nil), &evidence)
	if !evidence.Attempt.Current || evidence.Attempt.RuntimeFreshness != "expired" {
		t.Fatalf("renewal manufactured activity freshness=%#v", evidence.Attempt)
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, config)
	now = now.Add(90*time.Second + time.Nanosecond)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", other, map[string]any{"id": "other-machine", "hostname": "other-machine", "version": "test", "capacity": 1}), http.StatusOK)
	refused := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", other, tracker.NativeClaim{PolicyID: lease.PolicyID, WorkItemID: issue.WorkItemID, MachineID: "other-machine", SessionID: "other-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, refused, http.StatusConflict)
	other = worker
	replacement := claimNativeAttempt(t, f, other, "first-machine", "other-session", issue.WorkItemID)
	if replacement.FencingToken <= lease.FencingToken {
		t.Fatal("lease reassignment did not advance fencing")
	}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime?native_attempt_id="+event.Data.AttemptID, other, nil), &evidence)
	if evidence.Attempt.Current || evidence.Attempt.Status != "interrupted" || evidence.Attempt.RuntimeFreshness != "expired" || evidence.Attempt.Runtime.Activity.Dropped != 3 || evidence.Attempt.Runtime.Activity.Unpaired != 2 || evidence.CurrentLease == nil || evidence.CurrentLease.ID != replacement.ID || evidence.Scheduling.Outcome != "claimed" {
		t.Fatalf("restart lost runtime observation=%#v", evidence.Attempt)
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
	next.IdempotencyKey = "replacement-start"
	next.Data.RunID = event.Data.RunID
	next.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 28, Phase: "rework", HeartbeatAt: now}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", other, next), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime?attempt_id=168", other, nil), http.StatusUnprocessableEntity)
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
	var recovery tracker.NativeRecovery
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", other, nil), &recovery)
	if len(recovery.Attempts) != 2 || recovery.Attempts[0].Checkpoint == nil || recovery.Attempts[0].Checkpoint.EffectState != "ambiguous" || recovery.Attempts[0].MachineID != "first-machine" || recovery.Attempts[1].AttemptID != next.Data.AttemptID || recovery.Attempts[1].Checkpoint != nil {
		t.Fatalf("fresh startup lost the previous owned checkpoint: %+v", recovery.Attempts)
	}
	if len(recovery.Discussion) != 2 || recovery.Discussion[1].Body != "operator collaboration" || len(recovery.History) != 0 {
		t.Fatalf("compact recovery lost current human context: %+v", recovery)
	}
}

func TestNativeConcurrentOrderedEvents(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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

func TestNativeDispositionValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		summary string
		valid   bool
	}{
		{"legacy empty summary", "", true},
		{"exact bound", strings.Repeat("x", workpad.MaxFinalSummaryBytes), true},
		{"oversized summary", strings.Repeat("x", workpad.MaxFinalSummaryBytes+1), false},
		{"invalid UTF-8 summary", string([]byte{0xff}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}, Disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation", FinalSummary: test.summary}}
			if err := validateNativeExecution(data, "run.finished"); (err == nil) != test.valid {
				t.Fatalf("disposition validation = %v, want valid %t", err, test.valid)
			}
		})
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

func TestNativeAttemptPageBounds(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "large-attempts")
	worker := f.worker(t, "worker")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	output := strings.Repeat("<", 64<<10)
	var fences []tracker.FencingToken
	var first tracker.NativeLease
	for index := range 16 {
		if index > 0 {
			id := newNativeID("lease")
			result, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
 (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
 SELECT ?, issue_id, machine_id, ?, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at FROM leases WHERE lease_id = ?`, id, fmt.Sprintf("historical-%d", index), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
 (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, checkpoint_json, artifact_ids_json, started_at, updated_at, work_item_revision, dispatch_generation)
 SELECT ?, organization_id, project_id, work_item_id, ?, ?, ?, sequence, status,
 json_set(data_json, '$.attempt_id', ?, '$.lease_id', ?, '$.fencing_token', ?, '$.run_id', ?),
 checkpoint_json, artifact_ids_json, started_at, updated_at, work_item_revision, dispatch_generation
 FROM native_attempts WHERE lease_id = ?`, fmt.Sprintf("attempt_historical_%d", index), id, fence, fmt.Sprintf("run_historical_%d", index), fmt.Sprintf("attempt_historical_%d", index), id, strconv.FormatInt(fence, 10), fmt.Sprintf("run_historical_%d", index), first.ID)
			if err != nil {
				t.Fatal(err)
			}
			fences = append(fences, tracker.FencingToken(fence))
			continue
		}
		lease := claimNativeAttempt(t, f, worker, "machine", fmt.Sprintf("session-%d", index), issue.WorkItemID)
		first = lease
		start := nativeStartedEvent(lease)
		start.IdempotencyKey = fmt.Sprintf("start-%d", index)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
		checkpoint := start
		checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", fmt.Sprintf("checkpoint-%d", index), 2
		checkpoint.Data.Handoff = nativeTestCheckpoint()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		finish := start
		finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", fmt.Sprintf("finish-%d", index), 3, "succeeded"
		finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true, FinalSummary: strings.Repeat("x", workpad.MaxFinalSummaryBytes)}
		finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(),
			Validation: &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: output},
			Landing:    &tracker.NativeLandingReceipt{Waiting: true, HeadSHA: strings.Repeat("a", 40)}}
		if err := validateNativeRuntime(finish.Data.Runtime); err != nil {
			t.Fatalf("fixture is not a legitimate runtime: %v", err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		fences = append(fences, lease.FencingToken)
	}
	var full []tracker.NativeAttempt
	for _, view := range []string{"", "blockers"} {
		t.Run("view="+view, func(t *testing.T) {
			params := url.Values{"limit": {"100"}}
			if view != "" {
				params.Set("view", view)
			}
			var seen []tracker.FencingToken
			pages := 0
			for {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+params.Encode(), worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				budget := 1 << 20
				if view == "blockers" {
					budget = operatortool.WorkHistoryPageBytes
				}
				if response.Body.Len() > budget {
					t.Fatalf("page bytes=%d, budget=%d", response.Body.Len(), budget)
				}
				var page tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &page)
				pages++
				for _, attempt := range page.Items {
					seen = append(seen, attempt.FencingToken)
					if attempt.Status != "succeeded" || attempt.Identity == nil || attempt.WorkItemRevision != issue.Revision || attempt.StartedAt.IsZero() || attempt.Disposition == nil || !attempt.Disposition.HumanAction || len(attempt.Disposition.FinalSummary) != workpad.MaxFinalSummaryBytes || !reflect.DeepEqual(attempt.Checkpoint, nativeTestCheckpoint()) {
						t.Fatalf("lost authority/checkpoint at fence %d", attempt.FencingToken)
					}
					if view == "blockers" {
						if attempt.Runtime != nil {
							t.Fatal("blocker projection included runtime")
						}
					} else {
						if attempt.Runtime == nil || attempt.Runtime.Validation.Output != output || attempt.Runtime.Landing == nil || !attempt.Runtime.Landing.Waiting || attempt.Runtime.Landing.HeadSHA != strings.Repeat("a", 40) {
							t.Fatal("full attempt lost validation or landing evidence")
						}
						full = append(full, attempt)
					}
				}
				if len(seen) > len(fences) {
					t.Fatal("repeated attempts")
				}
				if page.NextCursor == "" {
					break
				}
				if pages == 1 {
					replay := url.Values{"limit": {"1"}, "cursor": {page.NextCursor}}
					if view != "" {
						replay.Set("view", view)
					}
					r := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+replay.Encode(), worker, nil)
					requireNativeStatus(t, r, http.StatusOK)
					var next tracker.Page[tracker.NativeAttempt]
					decodeHubResponse(t, r, &next)
					if len(next.Items) != 1 || next.Items[0].FencingToken != fences[len(seen)] {
						t.Fatal("byte boundary skipped a fence")
					}
					if view == "blockers" {
						replay.Del("view")
					} else {
						replay.Set("view", "blockers")
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+replay.Encode(), worker, nil), http.StatusUnprocessableEntity)
				}
				params.Set("cursor", page.NextCursor)
			}
			if pages < 2 || !reflect.DeepEqual(seen, fences) {
				t.Fatalf("pages=%d, fences=%v, want=%v", pages, seen, fences)
			}
		})
	}
	raw, err := json.Marshal(tracker.Page[tracker.NativeAttempt]{Items: full})
	if err != nil || len(raw) <= 1<<20 {
		t.Fatalf("old count-only payload bytes=%d, error=%v", len(raw), err)
	}
	t.Logf("legitimate count-only attempt payload=%d bytes", len(raw))
	for _, query := range []string{"view=unknown", "view=blockers&view=blockers"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+query, worker, nil), http.StatusUnprocessableEntity)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &detail)
	omitted := false
	for _, event := range detail.Items {
		if event.Type == "run.finished" && event.DataOmission != nil {
			omitted = true
		}
	}
	if !omitted {
		t.Fatal("fixture did not exercise large history data omission")
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var projected tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &projected)
	found := false
	for _, event := range projected.Items {
		if event.Type == "run.finished" {
			found = event.DataOmission == nil && event.Data.Run != nil && event.Data.Run.FencingToken == first.FencingToken && event.Data.Run.AttemptID != "" && event.Data.Run.Runtime == nil
		}
	}
	if !found {
		t.Fatal("blocker history omitted recorded run authority")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events
 (id, organization_id, project_id, work_item_id, sequence, type, schema_version, actor_json, data_json, recorded_at)
 SELECT ?, organization_id, project_id, work_item_id, sequence+100, 'issue.edited', schema_version,
 '{"kind":"human","principal_id":"operator"}', json_set(data_json, '$.revision', '9'), recorded_at
 FROM collaboration_events WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND type = 'run.finished'`, newNativeID("evt"), f.project.OrganizationID, f.project.ID, issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &projected)
	found = false
	for _, event := range projected.Items {
		if event.Type == "issue.edited" {
			found = event.Data.Revision == 9 && event.Actor.Kind == "human" && event.DataOmission == nil
		}
	}
	if !found {
		t.Fatal("large human edit lost invalidating revision")
	}
	for _, query := range []string{"view=unknown", "view=blockers&view=blockers"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?"+query, worker, nil), http.StatusUnprocessableEntity)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers&limit=1", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &projected)
	if projected.NextCursor == "" {
		t.Fatal("missing history continuation")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?cursor="+url.QueryEscape(projected.NextCursor), worker, nil), http.StatusUnprocessableEntity)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.runtime.validation.output', ?) WHERE lease_id = ?`, strings.Repeat("<", 256<<10), first.ID); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", worker, nil), http.StatusInternalServerError)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers&limit=1", worker, nil), http.StatusOK)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	denied := other.worker(t, "denied")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", denied, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers", denied, nil), http.StatusNotFound)
}
