package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type memoryRunDetails struct {
	mu          sync.Mutex
	data        map[string][]byte
	fail        bool
	uploads     int
	reads       int
	writerReads int
	afterStore  func(attachment.ArchiveReference)
	beforeRead  func(context.Context) error
}

func (m *memoryRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	m.mu.Lock()
	if m.fail {
		m.mu.Unlock()
		return attachment.ArchiveReference{}, errors.New("upload unavailable")
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		m.mu.Unlock()
		return ref, err
	}
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	if _, ok := m.data[ref.Key]; !ok {
		m.uploads++
	}
	m.data[ref.Key] = bytes.Clone(data)
	afterStore := m.afterStore
	m.mu.Unlock()
	if afterStore != nil {
		afterStore(ref)
	}
	return ref, nil
}

func (m *memoryRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if m.beforeRead != nil {
		if err := m.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	m.reads++
	if claimWriting(ctx) {
		m.writerReads++
		return nil, errors.New("detail read while claim writer is held")
	}
	data, ok := m.data[ref.Key]
	if !ok {
		return nil, attachment.ErrInvalid
	}
	return bytes.Clone(data), nil
}

func TestNativeRunDetailReferences(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	legacy := make(map[int64]tracker.NativeRunData)
	send := func(event tracker.NativeRunEvent, status int) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), status)
		for _, saved := range append(append([]tracker.NativeRunEvent{}, event.PriorEvents...), event) {
			data := saved.Data
			data.MachineID, data.SessionID = lease.MachineID, lease.SessionID
			legacy[data.Sequence] = data
		}
	}
	send(event, http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint-1", 2
	event.Data.Handoff = nativeTestCheckpoint()
	event.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 100, CachedInput: 20, Output: 10, CostEstimate: 1.5, Currency: "USD"}}
	send(event, http.StatusOK)
	first := event
	first.Type, first.IdempotencyKey, first.Data.Sequence, first.Data.Handoff = "run.observed", "observed-1", 3, nil
	first.Data.Usage = nil
	second := first
	second.IdempotencyKey, second.Data.Sequence = "observed-2", 4
	first.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: f.service.config.now(), Validation: &gate.CommandResult{Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: strings.Repeat("validation output\n", 3000)}}
	second.Data.Runtime = first.Data.Runtime
	event.IdempotencyKey, event.Data.Sequence, event.PriorEvents = "checkpoint-2", 5, []tracker.NativeRunEvent{first, second}
	event.Data.Runtime = first.Data.Runtime
	send(event, http.StatusOK)
	event.PriorEvents = nil
	event.IdempotencyKey, event.Data.Sequence = "checkpoint-3", 6
	send(event, http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "run.finished", "finish", 7, "succeeded", nil
	event.Data.CompletionBody = strings.Repeat("completion response\n", 2000)
	event.Data.Usage = nil
	send(event, http.StatusOK)
	send(event, http.StatusOK)
	var input, cached, output int64
	var cost float64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT input_tokens,cached_input_tokens,output_tokens,cost_estimate FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&input, &cached, &output, &cost); err != nil {
		t.Fatal(err)
	}
	if input != 100 || cached != 20 || output != 10 || cost != 1.5 {
		t.Fatalf("usage summary reset or double counted: %d/%d/%d cost=%g", input, cached, output, cost)
	}
	if store.uploads != 5 {
		t.Fatalf("uploaded %d segments, want five boundaries for seven events", store.uploads)
	}
	for _, table := range []string{"native_attempts", "collaboration_events"} {
		columns, err := f.service.database.reader.QueryContext(t.Context(), "SELECT name FROM pragma_table_xinfo(?) WHERE hidden=0", table)
		if err != nil {
			t.Fatal(err)
		}
		defer columns.Close()
		var sizes []string
		for columns.Next() {
			var name string
			if err := columns.Scan(&name); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, "length(CAST(coalesce("+strconv.Quote(name)+",'') AS BLOB))")
		}
		if err := columns.Err(); err != nil {
			t.Fatal(err)
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
		query := "SELECT max(" + strings.Join(sizes, "+") + ") FROM " + table
		if table == "collaboration_events" {
			query += " WHERE type LIKE 'run.%'"
		}
		var largest int
		if err := f.service.database.reader.QueryRowContext(t.Context(), query).Scan(&largest); err != nil || largest > 2048 {
			t.Fatalf("largest %s row=%d bytes, error=%v", table, largest, err)
		}
	}

	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var attempts tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, response, &attempts)
	if len(attempts.Items) != 1 || attempts.Items[0].CompletionBody != event.Data.CompletionBody || attempts.Items[0].Runtime.Validation.Output != first.Data.Runtime.Validation.Output || attempts.Items[0].Checkpoint == nil {
		t.Fatal("attempt detail was lost")
	}
	var raw string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var hydrated tracker.NativeRunData
	if err := decodeRunDetail(f.service.runDetailContext(t.Context()), raw, &hydrated); err != nil {
		t.Fatal(err)
	}
	expected := event.Data
	expected.MachineID, expected.SessionID = lease.MachineID, lease.SessionID
	got, err := json.Marshal(hydrated)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	var gotFields, wantFields map[string]json.RawMessage
	if err := json.Unmarshal(got, &gotFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantFields); err != nil {
		t.Fatal(err)
	}
	for field, value := range wantFields {
		if !reflect.DeepEqual(value, gotFields[field]) {
			t.Errorf("detail field %s differs from legacy output", field)
		}
	}
	for field := range gotFields {
		if _, ok := wantFields[field]; !ok {
			t.Errorf("unexpected detail field %s", field)
		}
	}
	reads := func() map[string]string {
		t.Helper()
		results := make(map[string]string)
		for _, suffix := range []string{"/attempts", "/attempts/" + event.Data.AttemptID, "/runtime", "/history?limit=100"} {
			response := performHubAPIRequest(t, f.service, http.MethodGet, path+suffix, worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			results[suffix] = response.Body.String()
		}
		ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
		executor := hostedOperatorExecutor{service: f.service}
		for _, name := range []string{operatortool.WorkRuns, operatortool.WorkAttemptReceipt, operatortool.GetNativeRun, operatortool.WorkHistory} {
			args := map[string]any{"project_id": f.project.ID, "reference": issue.WorkItemID}
			if name == operatortool.WorkAttemptReceipt {
				args["native_attempt_id"] = event.Data.AttemptID
			}
			if name == operatortool.GetNativeRun {
				args = map[string]any{"project_id": f.project.ID, "work_item_id": issue.WorkItemID, "attempt_id": event.Data.AttemptID}
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: encoded})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			results[name] = string(result.Content)
		}
		return results
	}
	referenced := reads()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	inline, err := json.Marshal(legacy[7])
	if err != nil {
		t.Fatal(err)
	}
	checkpointJSON, err := json.Marshal(nativeTestCheckpoint())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=?,checkpoint_json=? WHERE id=?", string(inline), string(checkpointJSON), event.Data.AttemptID); err != nil {
		t.Fatal(err)
	}
	var trigger string
	if err := tx.QueryRowContext(t.Context(), "SELECT sql FROM sqlite_schema WHERE name='collaboration_events_no_update'").Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "DROP TRIGGER collaboration_events_no_update"); err != nil {
		t.Fatal(err)
	}
	for sequence, data := range legacy {
		envelope := tracker.CollaborationData{Run: &data}
		if sequence == 7 {
			envelope.Revision = issue.Revision
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET data_json=? WHERE work_item_id=? AND json_extract(data_json,'$.run.sequence')=?", string(raw), issue.WorkItemID, strconv.FormatInt(sequence, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for name, inline := range reads() {
		if referenced[name] != inline {
			t.Errorf("%s differs between object references and legacy inline rows", name)
		}
	}

}

func TestNativeRunDetailUploadFailures(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: store})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "upload failure")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence, checkpoint.Data.Handoff = "run.checkpointed", "checkpoint", 2, nativeTestCheckpoint()
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	for _, event := range []tracker.NativeRunEvent{start, checkpoint, finish} {
		t.Run(event.Type, func(t *testing.T) {
			var before string
			err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(group_concat(data_json), '') FROM native_attempts").Scan(&before)
			if err != nil {
				t.Fatal(err)
			}
			store.fail = true
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event)
			requireNativeStatus(t, response, http.StatusInternalServerError)
			var after string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(group_concat(data_json), '') FROM native_attempts").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("failed upload changed the attempt or stored an inline fallback")
			}
			store.fail = false
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			var raw string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var stored storedRun
			if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Detail == nil || stored.Sequence != event.Data.Sequence {
				t.Fatalf("attempt failed to progress from reference after upload recovery: %v", err)
			}
		})
	}
}

func TestNativeRunDetailAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		released     bool
		expired      bool
		sequence     int64
		cancelled    bool
		body         string
		status       int
		duringUpload bool
		replay       bool
		batchGap     bool
	}{
		{name: "lease expires during upload", duringUpload: true, sequence: 2, status: http.StatusConflict},
		{name: "expired durable replay", expired: true, replay: true, status: http.StatusOK},
		{name: "released durable replay", released: true, replay: true, status: http.StatusOK},
		{name: "batch sequence gap", batchGap: true, sequence: 4, status: http.StatusConflict},
		{name: "expired checkpoint", expired: true, sequence: 2, status: http.StatusConflict},
		{name: "released checkpoint", released: true, sequence: 2, status: http.StatusConflict},
		{name: "skipped sequence", sequence: 3, status: http.StatusConflict},
		{name: "changed replay", sequence: 1, status: http.StatusConflict},
		{name: "released cancellation with content", released: true, cancelled: true, sequence: 2, body: "unowned completion", status: http.StatusConflict},
		{name: "released cancellation acknowledgment", released: true, cancelled: true, sequence: 2, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryRunDetails{}
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			original := event
			if test.released {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE lease_id=?", formatHubTime(now), lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.expired {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now), lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "next", test.sequence
			event.Data.Handoff = nativeTestCheckpoint()
			if test.cancelled {
				event.Type, event.Data.Outcome, event.Data.Handoff = "run.finished", "cancelled", nil
				event.Data.CompletionBody = test.body
			}
			if test.replay {
				event = original
			}
			if test.batchGap {
				observation := original
				observation.Type, observation.IdempotencyKey, observation.Data.Sequence = "run.observed", "observation", 2
				observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}
				event.Data.Runtime = observation.Data.Runtime
				event.PriorEvents = []tracker.NativeRunEvent{observation}
			}
			if test.duringUpload {
				store.afterStore = func(ref attachment.ArchiveReference) {
					data, err := store.ReadDetail(t.Context(), string(f.project.OrganizationID), ref)
					if err != nil {
						t.Fatal(err)
					}
					if len(data) == 0 {
						t.Fatal("uploaded detail unavailable during upload callback")
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(now), lease.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), test.status)
			wantUploads := 1
			if test.duringUpload {
				wantUploads = 2
			}
			if store.uploads != wantUploads {
				t.Fatalf("rejected or identity-only event uploaded detail: %d uploads", store.uploads)
			}
			var references int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_archive_references").Scan(&references); err != nil {
				t.Fatal(err)
			}
			if references != 1 {
				t.Fatalf("rejected event committed %d references", references)
			}
			var sequence int64
			var status string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence,status FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&sequence, &status); err != nil {
				t.Fatal(err)
			}
			if test.cancelled && test.status == http.StatusOK {
				if sequence != 2 || status != "cancelled" {
					t.Fatalf("cancellation acknowledgment lost: %d %s", sequence, status)
				}
			} else if sequence != 1 || status != "running" {
				t.Fatalf("rejected event changed attempt: %d %s", sequence, status)
			}
		})
	}
}

func TestNativeRunDetailIsolation(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: store})
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other organization"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	other := newNativeFixture(t, f.service, organization.ID, "other-project")
	refs := map[string]bool{}
	attempts := map[string]bool{}
	for _, fixture := range []nativeFixture{f, other} {
		approveHubTestPolicy(t, fixture.service, fixture.base+"/policy", hubTestPolicy())
		issue := fixture.create(t, "isolated work")
		worker := fixture.worker(t, "worker-"+string(fixture.project.OrganizationID))
		path := fixture.base + "/work-items/" + string(issue.WorkItemID)
		for _, session := range []string{"first", "second"} {

			lease := claimNativeAttempt(t, fixture, worker, "machine-"+string(fixture.project.OrganizationID), string(fixture.project.OrganizationID)+"-"+session, issue.WorkItemID)
			event := nativeStartedEvent(lease)
			event.IdempotencyKey = "start-" + session
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			if session == "first" {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence, checkpoint.Data.Handoff = "run.checkpointed", "checkpoint-"+session, 2, nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled", CancelAttempt: true}), http.StatusNoContent)
			} else {
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-"+session, 2, "failed"
				event.Data.CompletionBody = string(fixture.project.OrganizationID) + " " + session
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			var raw string
			if err := fixture.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var stored storedRun
			if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Detail == nil {
				t.Fatal("missing immutable detail")
			}
			if refs[stored.Detail.AttachmentID] || attempts[event.Data.AttemptID] {
				t.Fatal("attempts shared an object or attempt identity")
			}
			refs[stored.Detail.AttachmentID], attempts[event.Data.AttemptID] = true, true
			response := performHubAPIRequest(t, fixture.service, http.MethodGet, path+"/attempts/"+event.Data.AttemptID, worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var read tracker.NativeAttempt
			decodeHubResponse(t, response, &read)
			if read.CompletionBody != event.Data.CompletionBody {
				t.Fatal("detail crossed attempt boundary")
			}
			if session == "first" {
				if read.Outcome != "cancelled" || read.Checkpoint == nil || read.TerminalFailure == nil || read.TerminalFailure.ErrorClass != "lease_released" {
					t.Fatal("released attempt lost retained checkpoint or terminal failure")
				}
				var outcome, failure string
				if err := fixture.service.database.reader.QueryRowContext(t.Context(), "SELECT final_outcome,failure_class FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&outcome, &failure); err != nil || outcome != "cancelled" || failure != "lease_released" {
					t.Fatalf("released typed state=%s/%s err=%v", outcome, failure, err)
				}
			}
			foreign := other
			if fixture.project.OrganizationID == other.project.OrganizationID {
				foreign = f
			}
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodGet, foreign.base+"/work-items/"+string(issue.WorkItemID)+"/attempts/"+event.Data.AttemptID, foreign.token, nil), http.StatusNotFound)
		}
		response := performHubAPIRequest(t, fixture.service, http.MethodGet, path+"/attempts", worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.Page[tracker.NativeAttempt]
		decodeHubResponse(t, response, &page)
		if len(page.Items) != 2 || page.Items[0].AttemptID == page.Items[1].AttemptID || page.Items[0].CompletionBody == page.Items[1].CompletionBody {
			t.Fatal("both isolated attempts were not usable")
		}
	}
}

func TestNativeBoardRefreshReadsRunDetailReferences(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "running work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint-1", 2
	event.Data.Handoff = nativeTestCheckpoint()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
	if store.uploads == 0 {
		t.Fatal("attempt detail was not stored as a reference")
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO native_board_dirty(work_item_id) VALUES(?)", issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	f.service.maintainNativeBoard(context.Background())
	var running, dirty int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT running,(SELECT count(*) FROM native_board_dirty) FROM native_board_items WHERE work_item_id=?", issue.WorkItemID).Scan(&running, &dirty); err != nil {
		t.Fatal(err)
	}
	if running != 1 || dirty != 0 {
		t.Fatalf("background refresh could not read the referenced run detail: running=%d dirty=%d", running, dirty)
	}
}

func TestNativeRuntimeRunDetailHistoryCost(t *testing.T) {
	t.Parallel()
	for _, history := range []int{1, 8, 64, 512} {
		for _, preserves := range []bool{false, true} {
			t.Run(strconv.Itoa(history)+"/preserves="+strconv.FormatBool(preserves), func(t *testing.T) {
				store := &memoryRunDetails{}
				f := newDefaultNativeFixture(t, Config{RunDetails: store})
				if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
					t.Fatal(err)
				}
				issue := f.create(t, "checkpoint history")
				registered := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				registered.enroll(t)
				credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var first, latest string
				for i := range history {
					checkpoint := nativeTestCheckpoint()
					checkpoint.WorktreeState, checkpoint.Resume = "clean", "fresh_checkout"
					if preserves && i == 0 {
						checkpoint.WorktreeState = "dirty"
					}
					data := tracker.NativeRunData{Sequence: 1, Handoff: checkpoint, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "example failure"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", LocalAttemptID: int64(i + 1), Activity: &workflowmetrics.ActivityProfile{SessionID: 17, ProviderThreadRef: "thread-example"}}}
					latest = seedHealthRateAttempt(t, tx, scope, issue, string(registered.binding.MachineID), "failed", data, f.service.config.now(), -1)
					if i == 0 {
						first = latest
					}
					data.AttemptID, data.RunID = latest, latest
					if err := tx.QueryRowContext(t.Context(), "SELECT lease_id,fencing_token FROM native_attempts WHERE id=?", latest).Scan(&data.LeaseID, &data.FencingToken); err != nil {
						t.Fatal(err)
					}
					runs := map[string]tracker.NativeRunData{runDetailEntry(data): data}
					raw, err := json.Marshal(runs)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := store.StoreDetail(t.Context(), string(scope.organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					ctx := f.service.runDetailContext(t.Context())
					owner := ctx.Value(runDetailContextKey{}).(*runDetailContext)
					owner.organization, owner.reference, owner.runs = string(scope.organization), &ref, runs
					encoded, err := encodeRunDetail(ctx, data, false)
					if err != nil {
						t.Fatal(err)
					}
					checkpointJSON, err := encodeRunCheckpoint(ctx, data)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=?,checkpoint_json=?,checkpoint_preserves_source=? WHERE id=?", encoded, checkpointJSON, data.Handoff.PreservesSource(), latest); err != nil {
						t.Fatal(err)
					}
					if err := updateAttemptHotProjection(ctx, tx, latest, "failed", data); err != nil {
						t.Fatal(err)
					}
					if err := recordRunDetailReference(ctx, tx, scope, issue.WorkItemID, data, formatHubTime(f.service.config.now())); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				var writer *writerTiming
				f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						err := next(c)
						if strings.HasSuffix(c.Path(), "/explanation") {
							f.service.recordHostedRequest(c, time.Now())
							writer = writerTimingFrom(c.Request().Context())
						}
						return err
					}
				})
				counter := installReadQueryCounter(t, f.service.database)
				wantReads := 1
				if preserves && history > 1 {
					wantReads = 2
				}
				for _, endpoint := range []string{"runtime", "explanation"} {
					store.mu.Lock()
					store.reads = 0
					store.mu.Unlock()
					beforeQueries := counter.queries.Load()
					started := time.Now()
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/"+endpoint, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					store.mu.Lock()
					reads := store.reads
					store.mu.Unlock()
					t.Logf("%s history=%d preserves=%v reads=%d modeled_serial_latency=%s", endpoint, history, preserves, reads, time.Duration(reads)*250*time.Millisecond)
					if endpoint == "explanation" {
						var result explain.IssueExplanation
						decodeHubResponse(t, response, &result)
						if result.Attempt == nil || result.Attempt.NativeID != latest || !result.Found || result.Identity.IssueID != string(issue.WorkItemID) {
							t.Fatal("explanation identity or latest attempt lost")
						}
						if result.Sessions.Detent == nil || result.Sessions.Detent.ID != "17" || result.Sessions.Provider == nil || result.Sessions.Provider.ID != "thread-example" {
							t.Fatalf("session identities lost: %+v", result.Sessions)
						}
						failures, partial, err := readExplanationFailures(context.WithValue(t.Context(), explanationReadKey{}, true), f.service.database.reader, scope, string(issue.WorkItemID))
						signature := latestNativeFailureSignature(failures, partial)
						if err != nil || signature == nil || signature.Signature != "example failure" || signature.Count != min(history, 64) || signature.Partial != (history > 64) {
							t.Fatalf("bounded failure signature: %+v error=%v", signature, err)
						}
						if writer == nil || writer.txs.Load() != 0 {
							t.Fatalf("explanation acquired writer: timing=%+v", writer)
						}
						queries := counter.queries.Load() - beforeQueries
						if queries > 180 || response.Body.Len() > 64*1024 {
							t.Fatalf("unbounded explanation: queries=%d bytes=%d", queries, response.Body.Len())
						}
						t.Logf("explanation history=%d queries=%d bytes=%d elapsed=%s writer_txs=%d", history, queries, response.Body.Len(), time.Since(started), writer.txs.Load())
					}
					if endpoint == "runtime" {
						var evidence tracker.NativeRuntimeEvidence
						decodeHubResponse(t, response, &evidence)
						if evidence.Attempt == nil || evidence.Attempt.AttemptID != latest {
							t.Fatal("latest attempt lost")
						}
						if preserves && (evidence.SourceRecovery == nil || evidence.SourceRecovery.AttemptID != first) {
							t.Fatal("older preserved source lost")
						}
						if !preserves && evidence.SourceRecovery != nil {
							t.Fatal("clean checkpoint became preserved source")
						}
					}
					expectedReads := wantReads
					if endpoint == "explanation" {
						expectedReads = 0
					}
					if reads != expectedReads {
						t.Errorf("object reads grew with checkpoint history: %d", reads)
					}
				}

				var raw string
				var row int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT rowid,data_json FROM native_attempts WHERE id=?", latest).Scan(&row, &raw); err != nil {
					t.Fatal(err)
				}
				payload := storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: "native_attempts", Row: row, Data: json.RawMessage(raw)}
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				ref, compressed, err := attachment.PrepareArchive(string(scope.organization), 1, bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				converted, err := compactStorageJSON([]byte(raw), storageJSONReference{Organization: payload.Organization, Project: payload.Project, Family: payload.Family, Row: row, Reference: ref})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=? WHERE id=?", string(converted), latest); err != nil {
					t.Fatal(err)
				}
				objects := &changeObjectFixture{objects: map[string][]byte{ref.Key: compressed}}
				contentReads := 0
				f.service.config.content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
					contentReads++
					return objects.call(ctx, request)
				}
				for _, endpoint := range []string{"runtime", "explanation"} {
					store.mu.Lock()
					store.reads = 0
					store.mu.Unlock()
					contentReads = 0
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/"+endpoint, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					store.mu.Lock()
					reads := store.reads
					store.mu.Unlock()
					t.Logf("%s converted history=%d detail_reads=%d content_reads=%d", endpoint, history, reads, contentReads)
					expectedReads, expectedContent := wantReads, 1
					if endpoint == "explanation" {
						expectedReads, expectedContent = 0, 0
					}
					if reads != expectedReads || contentReads != expectedContent {
						t.Errorf("converted runtime read cost: details=%d content=%d", reads, contentReads)
					}
				}
			})
		}
	}
}

func TestRunDetailSegmentResolvesPreparedReferenceBeforeCommit(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: store})
	organization := "org_prepared"
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	prepared := map[string]tracker.NativeRunData{"attempt_prepared:2": {AttemptID: "attempt_prepared", Sequence: 2}}
	owner := &runDetailContext{query: f.service.database.reader, store: store, organization: organization, cache: map[string]map[string]tracker.NativeRunData{}, reuse: map[string]*runDetailReference{}, reference: &ref, runs: prepared}
	ctx := context.WithValue(t.Context(), runDetailContextKey{}, owner)
	runs, err := readRunDetailSegment(ctx, newRunDetailReference(ref, organization, "attempt_prepared:2"))
	if err != nil || runs["attempt_prepared:2"].Sequence != 2 {
		t.Fatalf("prepared detail before commit = %v, %v", runs, err)
	}
}
