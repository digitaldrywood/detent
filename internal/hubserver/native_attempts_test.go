package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
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

func nativeAttemptListingFixture(t *testing.T, count, bodyBytes int) (nativeFixture, tracker.NativeIssue, []tracker.NativeRunData) {
	t.Helper()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "attempt listing")
	worker := f.worker(t, "listing worker")
	lease := claimNativeAttempt(t, f, worker, "listing-machine", "listing-session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	var records []tracker.NativeRunData
	for index := range count {
		data := start.Data
		data.CompletionBody = strings.Repeat("x", bodyBytes)
		if index > 0 {
			data.LeaseID = tracker.LeaseID(fmt.Sprintf("lease_listing_%d", index))
			result, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
 (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
 SELECT ?, issue_id, machine_id, ?, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at FROM leases WHERE lease_id = ?`, data.LeaseID, fmt.Sprintf("listing-session-%d", index), lease.ID)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			data.FencingToken = tracker.FencingToken(fence)
			data.AttemptID = fmt.Sprintf("attempt_listing_%d", index)
			data.RunID = fmt.Sprintf("run_listing_%d", index)
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = ? WHERE id = ?`, string(encoded), data.AttemptID)
		} else {
			_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
 (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, checkpoint_json, artifact_ids_json, started_at, updated_at, work_item_revision, dispatch_generation)
 SELECT ?, organization_id, project_id, work_item_id, ?, ?, ?, sequence, status, ?, checkpoint_json, artifact_ids_json, started_at, updated_at, work_item_revision, dispatch_generation FROM native_attempts WHERE id = ?`, data.AttemptID, data.LeaseID, data.FencingToken, data.RunID, string(encoded), start.Data.AttemptID)
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, data)
	}
	return f, issue, records
}

func TestNativeAttemptsSkipUnusedHydration(t *testing.T) {
	for _, test := range []struct {
		name      string
		limit     int
		bodyBytes int
		corrupt   int
		want      int
	}{
		{"count lookahead", 1, 0, 1, 1},
		{"after byte boundary", 100, 400 << 10, 3, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, issue, records := nativeAttemptListingFixture(t, 4, test.bodyBytes)
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = '{"attempt_id":42}' WHERE id = ?`, records[test.corrupt].AttemptID); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/attempts?limit="+strconv.Itoa(test.limit), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.NativeAttempt]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != test.want || page.NextCursor == "" {
				t.Fatalf("items=%d cursor=%q, want %d and continuation", len(page.Items), page.NextCursor, test.want)
			}
		})
	}
}

func legacyNativeAttemptPage(attempts []tracker.NativeAttempt, limit, budget int, cursor nativeCursor, key []byte, view string) (tracker.Page[tracker.NativeAttempt], error) {
	page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
	hasMore := false
	for _, attempt := range attempts {
		if len(page.Items) == limit {
			hasMore = true
			break
		}
		next := cursor
		next.After = strconv.FormatInt(int64(attempt.FencingToken), 10)
		candidate := tracker.Page[tracker.NativeAttempt]{Items: append(page.Items, attempt)}
		var err error
		candidate.NextCursor, err = encodeNativeCursor(next, key)
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return page, err
		}
		if len(encoded)+1 > budget && len(page.Items) == 0 && view != "" {
			candidate.Items[0] = nativeAttemptAuthorityProjection(attempt)
			encoded, err = json.Marshal(candidate)
			if err != nil {
				return page, err
			}
		}
		if len(encoded)+1 > budget {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.NativeAttempt]{}, &nativeError{Code: "attempt_record_too_large", status: http.StatusUnprocessableEntity}
			}
			hasMore = true
			break
		}
		page.Items = candidate.Items
		cursor = next
	}
	if hasMore {
		var err error
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

func TestNativeAttemptPageByteBoundaryEquivalence(t *testing.T) {
	f, issue, records := nativeAttemptListingFixture(t, 5, 0)
	scope := nativeScope{organization: issue.OrganizationID, project: issue.ProjectID}
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/attempts"
	base, err := f.service.readAttempts(t.Context(), scope, string(issue.WorkItemID), url.Values{"limit": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"", "blockers", "recorded_blockers"} {
		for _, direction := range []string{"oldest", "newest"} {
			params := url.Values{"view": {view}, "direction": {direction}}
			_, cursor, key, err := f.service.readNativePage(t.Context(), scope, path, params)
			if err != nil {
				t.Fatal(err)
			}
			budget := 1 << 20
			if view != "" {
				budget = operatortool.WorkHistoryPageBytes
			}
			ordered := append([]tracker.NativeAttempt(nil), base.Items...)
			if direction == "newest" {
				for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
					ordered[left], ordered[right] = ordered[right], ordered[left]
				}
			}
			next := cursor
			next.After = strconv.FormatInt(int64(ordered[0].FencingToken), 10)
			nextCursor, err := encodeNativeCursor(next, key)
			if err != nil {
				t.Fatal(err)
			}
			first := ordered[0]
			first.CompletionBody = "x"
			one, err := json.Marshal(tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{first}, NextCursor: nextCursor})
			if err != nil {
				t.Fatal(err)
			}
			exact := budget - len(one)
			for _, size := range []int{0, 1000, exact - 1, exact, exact + 1, budget + 1} {
				body := strings.Repeat("x", size)
				for _, record := range records {
					record.CompletionBody = body
					encoded, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = ? WHERE id = ?`, string(encoded), record.AttemptID); err != nil {
						t.Fatal(err)
					}
				}
				for index := range ordered {
					ordered[index].CompletionBody = body
				}
				for _, limit := range []int{1, 2, 5, 100} {
					t.Run(fmt.Sprintf("%s/%s/bytes=%d/limit=%d", view, direction, size, limit), func(t *testing.T) {
						params := url.Values{"view": {view}, "direction": {direction}}
						params.Set("limit", strconv.Itoa(limit))
						remaining := ordered
						current := cursor
						for {
							want, wantErr := legacyNativeAttemptPage(remaining, limit, budget, current, key, view)
							got, err := f.service.readNativeAttemptPage(t.Context(), scope, string(issue.WorkItemID), params, json.Marshal)
							if wantErr != nil {
								var oversized *nativeError
								if !errors.As(err, &oversized) || oversized.Code != "attempt_record_too_large" || oversized.status != http.StatusUnprocessableEntity {
									t.Fatalf("error=%v, want oversized record", err)
								}
								break
							}
							if err != nil || !reflect.DeepEqual(got.Page, want) {
								t.Fatalf("page mismatch: items=%d want=%d error=%v", len(got.Items), len(want.Items), err)
							}
							encoded, err := json.Marshal(want)
							if err != nil || !bytes.Equal(got.encoded, append(encoded, '\n')) || len(got.encoded) > budget {
								t.Fatalf("response encoding differs: bytes=%d error=%v", len(got.encoded), err)
							}
							if want.NextCursor == "" {
								break
							}
							if len(want.Items) == 0 {
								t.Fatal("cursor did not advance")
							}
							remaining = remaining[len(want.Items):]
							current.After = strconv.FormatInt(int64(want.Items[len(want.Items)-1].FencingToken), 10)
							params.Set("cursor", want.NextCursor)
						}
					})
				}
			}
		}
	}
}

func TestNativeAttemptPageEncodesEachReturnedRecordOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		bodyBytes  int
		limit      int
		want       int
		considered int
	}{
		{"count limit", 0, 2, 2, 2},
		{"byte limit", 400 << 10, 100, 2, 3},
		{"last page", 1000, 100, 5, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, issue, _ := nativeAttemptListingFixture(t, 5, test.bodyBytes)
			counts := map[string]int{}
			marshal := func(value any) ([]byte, error) {
				attempt, ok := value.(tracker.NativeAttempt)
				if !ok {
					t.Fatalf("encoded growing page prefix: %T", value)
				}
				counts[attempt.AttemptID]++
				return json.Marshal(attempt)
			}
			got, err := f.service.readNativeAttemptPage(t.Context(), nativeScope{organization: issue.OrganizationID, project: issue.ProjectID}, string(issue.WorkItemID), url.Values{"limit": {strconv.Itoa(test.limit)}}, marshal)
			if err != nil || len(got.Items) != test.want || len(counts) != test.considered {
				t.Fatalf("items=%d encodes=%v error=%v", len(got.Items), counts, err)
			}
			for id, count := range counts {
				if count != 1 {
					t.Fatalf("attempt %s encoded %d times", id, count)
				}
			}
		})
	}
}

func BenchmarkNativeAttemptPageEncoding(b *testing.B) {
	for _, count := range []int{20, 100} {
		attempts := make([]tracker.NativeAttempt, count)
		for index := range attempts {
			attempts[index] = tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_%d", index), FencingToken: tracker.FencingToken(index + 1), CompletionBody: strings.Repeat("x", 6000)}}
		}
		cursor := nativeCursor{Version: 2, Scope: "example", Expires: 1800000000}
		key := []byte("example-cursor-key")
		b.Run(fmt.Sprintf("prefix/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				page, err := legacyNativeAttemptPage(attempts, count, 1<<20, cursor, key, "")
				if err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(page); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("incremental/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				page := nativeAttemptPage{encoded: []byte(`{"items":[`)}
				for _, attempt := range attempts {
					next := cursor
					next.After = strconv.FormatInt(int64(attempt.FencingToken), 10)
					nextCursor, err := encodeNativeCursor(next, key)
					if err != nil {
						b.Fatal(err)
					}
					encoded, err := json.Marshal(attempt)
					if err != nil || page.candidateBytes(encoded, nextCursor) > 1<<20 {
						b.Fatalf("unexpected page overflow or error: %v", err)
					}
					page.appendAttempt(attempt, encoded)
				}
				page.finish()
			}
		})
	}
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
	reconnect := finish
	reconnect.Type, reconnect.IdempotencyKey, reconnect.Data.Sequence, reconnect.Data.Outcome = "run.observed", "reconnect-terminal", 4, ""
	reconnect.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now()}
	invalid := reconnect
	invalid.IdempotencyKey, invalid.Data.Sequence = "invalid-observation", 2
	invalid.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 4, Generation: 9, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 99}, Identity: agentidentity.Identity{BackendID: "sentinel-private-value"}}
	invalidCheckpoint := invalid
	invalidCheckpoint.Type, invalidCheckpoint.IdempotencyKey, invalidCheckpoint.Data.Handoff = "run.checkpointed", "invalid-checkpoint", nativeTestCheckpoint()
	invalidValidation := invalid
	invalidValidation.IdempotencyKey = "invalid-validation"
	invalidValidation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Command: "sentinel-private-value", Output: "sentinel-private-output"}}

	lifecycleTest := t
	for _, test := range []struct {
		name     string
		event    tracker.NativeRunEvent
		status   int
		restart  bool
		revision int64
	}{
		{"evidence without upload", forged, http.StatusUnprocessableEntity, false, 0},
		{"invalid runtime observation", invalid, http.StatusUnprocessableEntity, false, 0},
		{"invalid checkpoint observation", invalidCheckpoint, http.StatusUnprocessableEntity, false, 0},
		{"invalid source validation", invalidValidation, http.StatusUnprocessableEntity, false, 0},
		{"checkpoint before start", checkpoint, http.StatusConflict, false, 0},
		{"start", start, http.StatusOK, false, 0},
		{"same command", start, http.StatusOK, false, 0},
		{"completion skips checkpoint", finish, http.StatusConflict, false, 0},
		{"checkpoint", checkpoint, http.StatusOK, false, 1},
		{"complete after restart", finish, http.StatusOK, true, 2},
		{"duplicate completion", finish, http.StatusOK, false, 2},
		{"completed attempt cannot reconnect", reconnect, http.StatusConflict, false, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.restart {
				config := f.service.config
				started := time.Now()
				if err := f.service.Shutdown(testenv.WaitContext(t, context.Background())); err != nil {
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
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, test.event)
			requireNativeStatus(t, response, test.status)
			var revision int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce((SELECT analytics_revision FROM native_attempts WHERE id=?),0)", start.Data.AttemptID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			if revision != test.revision {
				t.Fatalf("analytics revision=%d, want %d", revision, test.revision)
			}
			if strings.Contains(response.Body.String(), "sentinel-private") {
				t.Fatal("public rejection disclosed private payload")
			}
			if test.name == "invalid runtime observation" || test.name == "invalid checkpoint observation" || test.name == "invalid source validation" {
				var result struct {
					Details struct {
						Rejection struct{ Category, Field string }
					}
				}
				decodeHubResponse(t, response, &result)
				field := "data.runtime.activity"
				if test.name == "invalid source validation" {
					field = "data.runtime.validation"
				}
				if result.Details.Rejection.Category != "runtime_validation" || result.Details.Rejection.Field != field {
					t.Fatalf("lost structural rejection: %+v", result)
				}
			}
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
	var full tracker.Page[tracker.NativeAttempt]
	for _, view := range []string{"", "blockers", "recorded_blockers"} {
		response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+url.Values{"view": {view}}.Encode(), worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var attempts tracker.Page[tracker.NativeAttempt]
		decodeHubResponse(t, response, &attempts)
		if len(attempts.Items) != 1 || attempts.Items[0].Sequence != 3 || attempts.Items[0].Status != "succeeded" || attempts.Items[0].Checkpoint == nil || attempts.Items[0].Checkpoint.WorktreeState != "dirty" || attempts.Items[0].MachineID != lease.MachineID || attempts.Items[0].SessionID != lease.SessionID {
			t.Fatalf("view=%q attempt projection = %#v", view, attempts)
		}
		if attempts.Items[0].Runtime != nil || attempts.Items[0].RuntimeFreshness != "unavailable" {
			t.Fatalf("view=%q fabricated runtime evidence: %+v", view, attempts.Items[0])
		}
		if view == "" {
			full = attempts
		} else if !reflect.DeepEqual(attempts, full) {
			t.Fatalf("view=%q changed attempt without runtime", view)
		}
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE type LIKE 'run.%'").Scan(&count); err != nil || count != 3 {
		t.Fatalf("run event count = %d, error = %v", count, err)
	}
}

func TestNativeRecoveryAfterReassignmentAndRestart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		retained bool
		archive  bool
	}{
		{name: "expired attempt"},
		{name: "released retained source after runner loss", retained: true},
		{name: "unresolved closed source stays hot", retained: true, archive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }, RunDetails: &memoryRunDetails{}}
			f := newNativeFixture(t, openTestService(t, config), "", "recovery")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			firstMachine := "first-machine"
			var owner runnerFixture
			if test.retained {
				owner = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
				owner.enroll(t)
				worker, firstMachine = owner.redemption.Credential, string(owner.binding.MachineID)
			}
			claim := func(token, machine, session string) tracker.NativeLease {
				t.Helper()
				if !test.retained {
					return claimNativeAttempt(t, f, token, machine, session, issue.WorkItemID)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, tracker.NativeClaim{
					PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(machine), SessionID: session,
					TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				return lease
			}
			lease := claim(worker, firstMachine, "first-session")
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "push-ambiguity", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.ExternalEffect, event.Data.Handoff.EffectState, event.Data.Handoff.EffectID = "git_push", "ambiguous", newNativeID("effect")
			event.Data.Handoff.HeadSHA = strings.Repeat("a", 40)
			event.Data.Handoff.ExpectedHeadSHA = strings.Repeat("b", 40)
			var version tracker.ChangeVersion
			var bundle []byte
			var originalCheckout, successorCheckout, changePath string
			git := func(directory string, args ...string) string {
				t.Helper()
				command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Recovery Fixture", "-c", "user.email=recovery@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
				for _, value := range os.Environ() {
					if !strings.HasPrefix(value, "GIT_") {
						command.Env = append(command.Env, value)
					}
				}
				command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("recovery fixture git %v: %s: %v", args, output, err)
				}
				return strings.TrimSpace(string(output))
			}
			if test.retained {
				input := changeTestInput()
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				input.Repository = "https://github.com/example-org/example-project"
				bundle, originalCheckout = changeTestSourceCheckout(t, &input, []byte("retained recovery source\n"))
				event.Data.Handoff.HeadSHA = input.HeadSHA
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Retained recovery source", Body: "Source custody"})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				changePath = path + "/changes/" + change.ID
				response = performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "retained-source"}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				origin := t.TempDir()
				git(origin, "init", "--bare", "--object-format=sha1", "--template=")
				git(origin, "fetch", originalCheckout, input.BaseSHA+":refs/heads/main")
				git(origin, "symbolic-ref", "HEAD", "refs/heads/main")
				successorCheckout = t.TempDir()
				git(successorCheckout, "init", "-b", "main", "--object-format=sha1", "--template=")
				git(successorCheckout, "fetch", origin, "main")
				git(successorCheckout, "reset", "--hard", "FETCH_HEAD")
				git(successorCheckout, "remote", "add", "origin", input.Repository)
				git(successorCheckout, "config", "url."+origin+".insteadOf", input.Repository)
			}
			wantCheckpoint := *event.Data.Handoff
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 27, Phase: "implementation", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: now}}, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, Dropped: 3, Unpaired: 2, Coverage: "partial", AsOf: now}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			for _, elapsed := range []time.Duration{30 * time.Second, 70 * time.Second} {
				now = now.Add(elapsed)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			if test.retained {
				var raw, checkpoint string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json,checkpoint_json FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&raw, &checkpoint); err != nil {
					t.Fatal(err)
				}
				var run storedRun
				var storedCheckpoint storedRunCheckpoint
				if err := json.Unmarshal([]byte(raw), &run); err != nil || run.Detail == nil {
					t.Fatalf("attempt is not reference-backed: %v", err)
				}
				if err := json.Unmarshal([]byte(checkpoint), &storedCheckpoint); err != nil || storedCheckpoint.Detail == nil {
					t.Fatalf("checkpoint is not reference-backed: %v", err)
				}
			}
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime", worker, nil), &evidence)
			if !evidence.Attempt.Current || evidence.Attempt.RuntimeFreshness != "expired" {
				t.Fatalf("renewal manufactured activity freshness=%#v", evidence.Attempt)
			}
			if test.retained {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				if err := os.RemoveAll(originalCheckout); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, config)
			now = now.Add(90*time.Second + time.Nanosecond)
			if test.retained {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(now.Add(-runnerauth.HeartbeatTimeout-time.Minute)), owner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if test.archive {
				f.service.config.Content = func(context.Context, entryauthority.ContentRequest) ([]byte, error) {
					t.Error("unresolved source was archived")
					return nil, errors.New("unexpected publication")
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				move := func(state string, at time.Time) {
					t.Helper()
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					current, _, err := readNativeIssue(t.Context(), tx, scope, string(issue.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					from := current.State
					current.State = state
					if _, err := persistNativeIssue(t.Context(), tx, scope, current, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: state}, at); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				move("Done", now.Add(-73*time.Hour))
				if err := f.service.archiveClosedNativeIssues(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				var pointers int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_storage_archives WHERE work_item_id=?", issue.WorkItemID).Scan(&pointers); err != nil {
					t.Fatal(err)
				}
				if pointers != 0 {
					t.Fatal("unresolved Change was archived")
				}
				move("Todo", now)
			}
			refuseEvidence := func(mutation tracker.Mutation) {
				t.Helper()
				credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
				err = recordAttachmentEvidence(t.Context(), tx, scope, attachment.EvidenceRequest{Mutation: mutation, WorkItemID: issue.WorkItemID, AttemptID: event.Data.AttemptID, Caption: "Expired evidence"}, attachment.Metadata{}, now)
				want := "stale_execution"
				if test.retained && mutation.LeaseID != "" && mutation.LeaseID != lease.ID {
					want = "not_found"
				}
				var failure *nativeError
				if !errors.As(err, &failure) || failure.Code != want {
					t.Fatalf("stale evidence was not fenced: %v", err)
				}
			}
			refuseEvidence(tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken})
			other, successorMachine := worker, firstMachine
			if test.retained {
				successor := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
				successor.enroll(t)
				other, successorMachine = successor.redemption.Credential, string(successor.binding.MachineID)
				if successorMachine == firstMachine || successor.binding.RunnerID == owner.binding.RunnerID {
					t.Fatal("recovery reused the original runner")
				}
			}
			replacement := claim(other, successorMachine, "other-session")
			if replacement.FencingToken <= lease.FencingToken {
				t.Fatal("lease reassignment did not advance fencing")
			}
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime?native_attempt_id="+event.Data.AttemptID, other, nil), &evidence)
			if evidence.Attempt.Current || evidence.Attempt.Status != "interrupted" || evidence.Attempt.RuntimeFreshness != "expired" || evidence.Attempt.Runtime.Activity.Dropped != 3 || evidence.Attempt.Runtime.Activity.Unpaired != 2 || evidence.CurrentLease == nil || evidence.CurrentLease.ID != replacement.ID || evidence.Scheduling.Outcome != "claimed" {
				t.Fatalf("restart lost runtime observation=%#v", evidence.Attempt)
			}
			if test.retained && (evidence.Attempt.ClaimReleasedAt == nil || !reflect.DeepEqual(evidence.Attempt.Checkpoint, &wantCheckpoint)) {
				t.Fatalf("released attempt lost its exact checkpoint: %+v", evidence.Attempt)
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
					refuseEvidence(test.mutation)
				})
			}
			refuseEvidence(tracker.Mutation{LeaseID: replacement.ID, FencingToken: replacement.FencingToken})
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
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?direction=newest&limit=1", other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].FencingToken != replacement.FencingToken || page.NextCursor == "" {
				t.Fatalf("newest attempt page = %#v", page)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?direction=newest&limit=1&cursor="+url.QueryEscape(page.NextCursor), other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			page = tracker.Page[tracker.NativeAttempt]{}
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].FencingToken != lease.FencingToken || page.NextCursor != "" {
				t.Fatalf("older attempt page = %#v", page)
			}
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", other, nil), &recovery)
			if len(recovery.Attempts) != 2 || recovery.Attempts[0].Checkpoint == nil || recovery.Attempts[0].Checkpoint.EffectState != "ambiguous" || string(recovery.Attempts[0].MachineID) != firstMachine || recovery.Attempts[1].AttemptID != next.Data.AttemptID || recovery.Attempts[1].Checkpoint != nil {
				t.Fatalf("fresh startup lost the previous owned checkpoint: %+v", recovery.Attempts)
			}
			if test.retained {
				sourceAttempt := recovery.SourceAttempt()
				if recovery.SourceAttemptID != event.Data.AttemptID || sourceAttempt == nil || !reflect.DeepEqual(sourceAttempt.Checkpoint, &wantCheckpoint) || recovery.ChangeDetail == nil || len(recovery.ChangeDetail.Versions) != 1 {
					t.Fatalf("successor lost retained source custody: %+v", recovery)
				}
				recoveredVersion := recovery.ChangeDetail.Versions[0]
				if recoveredVersion.ID != version.ID || !reflect.DeepEqual(recoveredVersion.Source, version.Source) {
					t.Fatal("recovery selected a different immutable source")
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, changePath+"/versions/"+recoveredVersion.ID+"/source", other, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), bundle) {
					t.Fatal("successor retrieved different retained source bytes")
				}
				backend, err := workspace.NewLocalGitContext(t.Context(), workspace.LocalGitOptions{SourceRoot: successorCheckout, Root: filepath.Join(t.TempDir(), "workspaces"), AutoBranch: true, CommitIdentity: workspace.CommitIdentity{Name: "Recovery Fixture", Email: "recovery@example.invalid"}})
				if err != nil {
					t.Fatal(err)
				}
				restored, err := backend.Create(t.Context(), workspace.Issue{Identifier: "native#1", NativeRework: true, BaseRef: recoveredVersion.BaseSHA, Source: &workspace.ChangeSource{Version: recoveredVersion, Bundle: response.Body.Bytes()}})
				if err != nil {
					t.Fatalf("restore retained source on successor: %v", err)
				}
				if git(restored.Path, "rev-parse", "HEAD") != wantCheckpoint.HeadSHA {
					t.Fatal("restored source differs from checkpoint head")
				}
				content, err := os.ReadFile(filepath.Join(restored.Path, "source.txt"))
				if err != nil || string(content) != "retained recovery source\n" {
					t.Fatalf("restored source bytes=%q error=%v", content, err)
				}
			}
			if len(recovery.Discussion) != 2 || recovery.Discussion[1].Body != "operator collaboration" || len(recovery.History) != 0 {
				t.Fatalf("compact recovery lost current human context: %+v", recovery)
			}
		})
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
		{"git checkpoint without commit", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.TreeSHA = "git_ref", tracker.CheckpointRefPrefix+"wi_1", strings.Repeat("e", 40)
		}},
		{"git checkpoint outside the Detent namespace", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = "git_ref", "refs/heads/main", strings.Repeat("c", 40), strings.Repeat("e", 40)
		}},
		{"git checkpoint ref traversal", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = "git_ref", tracker.CheckpointRefPrefix+"..", strings.Repeat("c", 40), strings.Repeat("e", 40)
		}},
		{"local checkpoint claiming a git ref", func(d *tracker.NativeRunData) {
			d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = tracker.CheckpointRefPrefix+"wi_1", strings.Repeat("c", 40), strings.Repeat("e", 40)
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
	for index := range 51 {
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
		finish.Data.CompletionBody = "Completed attempt details"
		finish.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 123, Output: 45, Currency: "USD"}}
		finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true, FinalSummary: strings.Repeat("x", workpad.MaxFinalSummaryBytes)}
		finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(),
			Validation: &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: output},
			Landing:    &tracker.NativeLandingReceipt{Waiting: true, HeadSHA: strings.Repeat("a", 40)}}
		if err := validateNativeRuntime(finish.Data.Runtime); err != nil {
			t.Fatalf("fixture is not a legitimate runtime: %v", err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
		var dispositionStatus string
		var dispositionHumanAction bool
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT disposition_status,disposition_human_action FROM native_attempts WHERE id=?", finish.Data.AttemptID).Scan(&dispositionStatus, &dispositionHumanAction); err != nil {
			t.Fatal(err)
		}
		if dispositionStatus != "blocked" || !dispositionHumanAction {
			t.Fatalf("typed disposition=%s/%v", dispositionStatus, dispositionHumanAction)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		fences = append(fences, lease.FencingToken)
	}
	var full []tracker.NativeAttempt
	for _, view := range []string{"", "blockers", "recorded_blockers"} {
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
				if view != "" {
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
					if view != "" {
						if attempt.Runtime != nil || attempt.RuntimeFreshness != "unavailable" {
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
	if full == nil {
		t.Fatal("full attempt view returned no records")
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	for _, transport := range []string{"stdio", "http"} {
		for _, limit := range []int{1, 20, 200} {
			t.Run(fmt.Sprintf("work_runs/%s/%d", transport, limit), func(t *testing.T) {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				args := map[string]any{"project_id": string(f.project.ID), "reference": string(issue.WorkItemID), "limit": limit}
				seen := 0
				for {
					raw := hostedContextData(t, call("tools/call", operatortool.WorkRuns, args), false)
					var result operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					if len(raw) > operatortool.MaxResultBytes || len(result.Data.Items) == 0 || len(result.Data.Items) > limit {
						t.Fatalf("invalid run page: bytes=%d items=%d", len(raw), len(result.Data.Items))
					}
					for _, attempt := range result.Data.Items {
						if seen >= len(full) {
							t.Fatal("repeated attempts")
						}
						want := full[seen]
						want.Runtime = nil
						want.RuntimeFreshness = "unavailable"
						if !reflect.DeepEqual(attempt, want) {
							t.Fatalf("summary changed fields at attempt %d", seen)
						}
						seen++
					}
					if result.Data.NextCursor == "" {
						break
					}
					args["cursor"] = result.Data.NextCursor
				}
				if seen != len(full) {
					t.Fatalf("read %d attempts, want %d", seen, len(full))
				}
			})
		}
	}
	if len(full) == 0 {
		t.Fatal("missing full attempt fixtures")
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
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", worker, nil)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if len(full) == 0 {
		t.Fatal("missing full attempt for oversized record identity check")
	}
	if !strings.Contains(response.Body.String(), string(issue.WorkItemID)) || !strings.Contains(response.Body.String(), full[0].AttemptID) {
		t.Fatal("oversized record refusal lost item or attempt identity")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers&limit=1", worker, nil), http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.completion_body', ?) WHERE lease_id = ?`, strings.Repeat("<", 64<<10), first.ID); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"blockers", "recorded_blockers"} {
		t.Run("oversized_pagination/"+view, func(t *testing.T) {
			params := url.Values{"view": {view}, "limit": {"200"}}
			seen := 0
			for {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+params.Encode(), worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if response.Body.Len() > operatortool.WorkHistoryPageBytes {
					t.Fatalf("page exceeded budget: %d", response.Body.Len())
				}
				var page tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &page)
				if len(page.Items) == 0 || seen == 0 && len(page.Items) < 2 {
					t.Fatal("oversized attempt prevented neighboring attempts from being returned")
				}
				for _, got := range page.Items {
					if seen >= len(full) {
						t.Fatal("repeated attempt")
					}
					want := full[seen]
					want.Runtime, want.RuntimeFreshness = nil, "unavailable"
					if seen == 0 {
						want.CompletionBody, want.Usage = "", nil
						want.Unavailable = []string{"completion_body", "usage"}
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("attempt %d lost authority, checkpoint or unaffected detail", seen)
					}
					seen++
				}
				if page.NextCursor == "" {
					break
				}
				params.Set("cursor", page.NextCursor)
			}
			if seen != len(full) {
				t.Fatalf("returned %d attempts, want %d", seen, len(full))
			}
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+full[0].AttemptID, worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var retained tracker.NativeAttempt
	decodeHubResponse(t, response, &retained)
	if retained.CompletionBody != strings.Repeat("<", 64<<10) || retained.Runtime == nil || retained.Runtime.Validation.Output != strings.Repeat("<", 256<<10) || len(retained.Unavailable) != 0 {
		t.Fatal("projection changed retained attempt detail")
	}
	for _, transport := range []string{"stdio", "http"} {
		for _, limit := range []int{1, 20, 200} {
			t.Run(fmt.Sprintf("oversized_summary/%s/%d", transport, limit), func(t *testing.T) {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				args := map[string]any{"project_id": string(f.project.ID), "reference": string(issue.WorkItemID), "limit": limit}
				raw := hostedContextData(t, call("tools/call", operatortool.WorkRuns, args), false)
				var result operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if len(raw) > operatortool.MaxResultBytes || len(result.Data.Items) == 0 {
					t.Fatalf("unbounded or empty summary: bytes=%d", len(raw))
				}
				got := result.Data.Items[0]
				want := full[0]
				want.Runtime, want.RuntimeFreshness = nil, "unavailable"
				want.CompletionBody, want.Usage = "", nil
				if !reflect.DeepEqual(got.Unavailable, []string{"completion_body", "usage"}) {
					t.Fatal("omitted attempt fields were not marked unavailable")
				}
				got.Unavailable = nil
				if !reflect.DeepEqual(got, want) {
					t.Fatal("oversized summary changed authority or checkpoint fields")
				}
			})
		}
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	denied := other.worker(t, "denied")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", denied, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers", denied, nil), http.StatusNotFound)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var sequence int64
	var source string
	if err := tx.QueryRowContext(t.Context(), "SELECT max(sequence) FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM collaboration_events WHERE work_item_id=? AND type='run.finished' LIMIT 1", issue.WorkItemID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	for index := range 101 {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT ?,organization_id,project_id,work_item_id,?,type,schema_version,actor_json,data_json,recorded_at FROM collaboration_events WHERE id=?`, newNativeID("evt"), sequence+int64(index)+1, source); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=timeline&direction=newest&limit=100", worker, nil)
	t.Logf("newest timeline history page: elapsed=%s bytes=%d", time.Since(started), response.Body.Len())
	requireNativeStatus(t, response, http.StatusOK)
	var timeline tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &timeline)
	if len(timeline.Items) != 100 || timeline.NextCursor == "" || response.Body.Len() > 100<<10 {
		t.Fatalf("timeline page rows=%d bytes=%d cursor=%t", len(timeline.Items), response.Body.Len(), timeline.NextCursor != "")
	}
	for _, event := range timeline.Items {
		if event.Data.Run != nil || event.DataOmission != nil {
			t.Fatal("timeline loaded large run detail instead of the event envelope")
		}
	}

}
