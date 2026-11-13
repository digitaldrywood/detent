package hubserver

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type memoryRunDetails struct {
	mu          sync.Mutex
	data        map[string][]byte
	fail        bool
	uploads     int
	reads       int
	keyReads    map[string]int
	writerReads int
	afterStore  func(attachment.ArchiveReference)
	beforeRead  func(context.Context) error
}

func (m *memoryRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	m.mu.Lock()
	if m.fail {
		m.mu.Unlock()
		return attachment.ArchiveReference{}, errors.New("upload unavailable")
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
	if m.keyReads != nil {
		m.keyReads[ref.Key]++
	}
	if claimWriting(ctx) {
		m.writerReads++
		return nil, errors.New("detail read while claim writer is held")
	}
	data, ok := m.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}

func TestNativeRunDetailRetainsHostLandingTransition(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"", "Rework", "Done"} {
		t.Run(target, func(t *testing.T) {
			archived := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", LeaseID: "lease", FencingToken: 1, Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{VersionID: "version", FromState: "old", Gate: &gate.CommandResult{Output: "retained output"}}}}
			stored := storedRun{NativeRunData: compactRun(archived), Detail: &runDetailReference{AttachmentID: "attachment", Entry: "attempt:0"}}
			stored.Runtime.Landing.FromState, stored.Runtime.Landing.TargetState = "Merging", target
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), runDetailContextKey{}, &runDetailContext{cache: map[string]map[string]tracker.NativeRunData{"attachment": {"attempt:0": archived}}})
			var got tracker.NativeRunData
			if err := decodeRunDetail(ctx, nil, string(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Runtime.Landing.FromState != "Merging" || got.Runtime.Landing.TargetState != target || got.Runtime.Landing.Gate.Output != "retained output" {
				t.Fatalf("hydrated landing lost host transition or archive output: %+v", got.Runtime.Landing)
			}
			if archived.Runtime.Landing.FromState != "old" || archived.Runtime.Landing.TargetState != "" {
				t.Fatal("hydration mutated cached archive")
			}
		})
	}
}

func TestNativeRunDetailReferences(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
	repository := f.service.config.TenantRepository.(*postgres.Repository)
	admin := hostedPostgresAdmin(t, f.service)
	if f.service.config.Postgres.Pool.ServingConnections != 1 {
		t.Fatal("fixture must have one PostgreSQL serving connection")
	}
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events)
	runner.enroll(t)
	reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, "runner", runner.redemption.BackendIsolation, 2)
	worker := runner.redemption.Credential
	claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
		PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "session",
		TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
	})
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	checkSingleReader := func(name string, read func(context.Context) error) {
		t.Helper()
		t.Run("single reader/"+name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- read(ctx) }()
			wait := testenv.WaitContext(t, t.Context())
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-wait.Done():
				cancel()
				<-done
				t.Fatal("read did not finish with one PostgreSQL serving connection")
			}
		})
	}
	expectedHistory := make(map[int64]tracker.NativeRunData)
	send := func(event tracker.NativeRunEvent, status int) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), status)
		for _, saved := range append(append([]tracker.NativeRunEvent{}, event.PriorEvents...), event) {
			data := saved.Data
			data.MachineID, data.SessionID, data.RunnerID = lease.MachineID, lease.SessionID, runner.binding.RunnerID
			expectedHistory[data.Sequence] = data
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
	first.Data.Runtime.Phases = []tracker.NativePhase{{Name: "implementation", StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-time.Minute)}}
	second.Data.Runtime = first.Data.Runtime
	event.IdempotencyKey, event.Data.Sequence, event.PriorEvents = "checkpoint-2", 5, []tracker.NativeRunEvent{first, second}
	event.Data.Runtime = first.Data.Runtime
	send(event, http.StatusOK)
	event.PriorEvents = nil
	event.IdempotencyKey, event.Data.Sequence = "checkpoint-3", 6
	send(event, http.StatusOK)
	for _, suffix := range []string{"/routing", ""} {
		checkSingleReader("runner"+suffix, func(ctx context.Context) error {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, runner.identityPath()+suffix, nil)
			if suffix == "" {
				request = httptest.NewRequestWithContext(ctx, http.MethodGet, runner.base+"/runners", nil)
			}
			request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				return fmt.Errorf("request status %d: %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), string(lease.ID)) {
				return errors.New("runner routing lost the active lease")
			}
			return nil
		})
	}
	checkSingleReader("hosted runner", func(ctx context.Context) error {
		config := f.service.config
		config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
		service := &Service{database: f.service.database, config: config}
		credential, status, err := f.service.authenticateTenantAPIHash(ctx, f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
		if err != nil || status != http.StatusOK {
			return fmt.Errorf("authenticate hosted runner read status=%d: %w", status, err)
		}
		scope := nativeScope{organization: f.project.OrganizationID, credential: credential}
		routing, err := service.readRunnerRouting(f.service.runDetailContext(ctx), scope, runner.binding.RunnerID)
		if err != nil {
			return err
		}
		if len(routing.Leases) != 1 || routing.Leases[0].ID != lease.ID {
			return errors.New("hosted runner routing lost the active lease")
		}
		return nil
	})
	heartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", worker, map[string]any{"display_name": runner.redemption.DisplayName, "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation})
	requireNativeStatus(t, heartbeat, http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "run.finished", "finish", 7, "succeeded", nil
	event.Data.CompletionBody = strings.Repeat("completion response\n", 2000)
	event.Data.Usage = nil
	send(event, http.StatusOK)
	send(event, http.StatusOK)
	now = now.Add(time.Microsecond)
	var input, cached, output int64
	var cost float64
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.(reportstore.UsageReader).ReadReportUsage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{f.project.ID}, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 100})
		for _, row := range rows {
			if row.AttemptID == event.Data.AttemptID {
				input += row.Input
				cached += row.CachedInput
				output += row.Output
				cost += row.Cost
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if input != 100 || cached != 20 || output != 10 || cost != 1.5 {
		t.Fatalf("usage summary reset or double counted: %d/%d/%d cost=%g", input, cached, output, cost)
	}
	if store.uploads != 25 {
		t.Fatalf("uploaded %d objects, want nine issue and policy objects plus sixteen attempt, checkpoint, receipt and history payloads", store.uploads)
	}
	for _, table := range []string{"native_attempts", "collaboration_events"} {
		var largest int
		query := "SELECT COALESCE(max(pg_column_size(r)),0) FROM " + table + " r WHERE organization_id=$1"
		if table == "collaboration_events" {
			query += " AND type LIKE 'run.%'"
		}
		if err := admin.QueryRowContext(t.Context(), query, f.project.OrganizationID).Scan(&largest); err != nil || largest == 0 || largest > 2048 {
			t.Fatalf("largest %s row=%d bytes, error=%v", table, largest, err)
		}
	}

	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=detail", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var attempts tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, response, &attempts)
	if len(attempts.Items) != 1 || attempts.Items[0].CompletionBody != event.Data.CompletionBody || attempts.Items[0].Runtime.Validation.Output != first.Data.Runtime.Validation.Output || attempts.Items[0].Checkpoint == nil {
		t.Fatal("attempt detail was lost")
	}
	row := readRunDetailAttemptRecord(t, f, event.Data.AttemptID)
	snapshot, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), analyticsTestScope(t, f), string(issue.WorkItemID), event.Data.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	hydrated, err := f.service.readNativeAttemptRunTenant(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	checkActivity := func(stage string) {
		t.Helper()
		current, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), analyticsTestScope(t, f), string(issue.WorkItemID), event.Data.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		run, err := f.service.readNativeAttemptRunTenant(t.Context(), current)
		if err != nil || run.Runtime == nil || !reflect.DeepEqual(run.Runtime.Phases, first.Data.Runtime.Phases) {
			t.Fatalf("%s lost archived phases: %v", stage, err)
		}
		var facts activitystore.Snapshot
		if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			facts, err = r.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{f.project.ID}, From: now.Add(-time.Hour), To: now, Now: now, Limit: 100})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(facts.Durations) != 1 || facts.Durations[0].AttemptID != event.Data.AttemptID || facts.Durations[0].Seconds != 60 || facts.Durations[0].Partial || facts.DurationsPartial {
			t.Fatalf("%s duration facts=%+v partial=%v", stage, facts.Durations, facts.DurationsPartial)
		}
	}
	checkActivity("writer")

	expected := event.Data
	expected.MachineID, expected.SessionID, expected.RunnerID = lease.MachineID, lease.SessionID, runner.binding.RunnerID
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
			t.Errorf("detail field %s differs from submitted output", field)
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
	for _, suffix := range []string{"/runtime", "/attempts", "/attempts/" + event.Data.AttemptID, "/history?limit=100"} {
		checkSingleReader(suffix, func(ctx context.Context) error {
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, path+suffix, nil)
			request.Header.Set("Authorization", "Bearer "+worker)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				return fmt.Errorf("request status %d: %s", response.Code, response.Body.String())
			}
			return nil
		})
	}
	var history []eventstore.Event
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		history, err = r.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool)
	for _, record := range history {
		if !strings.HasPrefix(record.Type, "run.") {
			continue
		}
		entry, err := eventstore.NewContent(store).Event(t.Context(), record)
		if err != nil || entry.Data.Run == nil {
			t.Fatalf("retained history run unavailable: %v", err)
		}
		run := entry.Data.Run
		if !reflect.DeepEqual(*run, expectedHistory[run.Sequence]) {
			t.Fatalf("history payload sequence=%d differs from independently submitted data", run.Sequence)
		}
		seen[run.Sequence] = true
	}
	if len(seen) != len(expectedHistory) || len(seen) != 7 {
		t.Fatalf("retained history sequences=%d want=7", len(seen))
	}
	referenced := reads()
	importRunDetailHistory(t, f, issue, row, expected)
	checkActivity("historical source replay against current attempt")
	for name, imported := range reads() {
		if referenced[name] != imported {
			t.Errorf("%s differs after historical source replay against current attempt", name)
		}
	}
	t.Run("empty target historical attempt and inline history", func(t *testing.T) {
		importRunDetailHistoryIntoEmptyTarget(t, issue, row, lease, expected, expectedHistory, now)
	})
}

func TestNativeRunDetailUploadFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		failure  string
		sequence int64
		status   int
	}{
		{"upload unavailable", 1, http.StatusServiceUnavailable},
		{"upload unavailable", 2, http.StatusServiceUnavailable},
		{"upload unavailable", 3, http.StatusServiceUnavailable},
		{"verification unavailable", 1, http.StatusServiceUnavailable},
		{"verification unavailable", 2, http.StatusServiceUnavailable},
		{"verification unavailable", 3, http.StatusServiceUnavailable},
		{"verification mismatch", 1, http.StatusInternalServerError},
		{"verification mismatch", 2, http.StatusInternalServerError},
		{"verification mismatch", 3, http.StatusInternalServerError},
	} {
		t.Run(fmt.Sprintf("%s/sequence=%d", test.failure, test.sequence), func(t *testing.T) {
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
			events := []tracker.NativeRunEvent{start, checkpoint, finish}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			for _, event := range events[:test.sequence-1] {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			}
			type durableState struct {
				present                           bool
				sequence                          int64
				status                            string
				usage, objects, commands, history int64
			}
			read := func() durableState {
				t.Helper()
				var state durableState
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.Attempt(t.Context(), start.Data.AttemptID)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					state.present = err == nil
					if state.present {
						state.sequence, state.status = row.Sequence, row.Status
					}
					state.usage, err = r.CollaborationBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM archive_objects WHERE organization_id=$1),(SELECT count(*) FROM native_command_states WHERE organization_id=$1),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)`, f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&state.objects, &state.commands, &state.history); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := read()
			switch test.failure {
			case "upload unavailable":
				store.fail = true
			case "verification unavailable":
				store.beforeRead = func(context.Context) error { return errors.New("read unavailable") }
			case "verification mismatch":
				store.afterStore = func(ref attachment.ArchiveReference) {
					store.mu.Lock()
					defer store.mu.Unlock()
					store.data[ref.Key] = []byte("different payload")
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, events[test.sequence-1]), test.status)
			if after := read(); after != before {
				t.Fatalf("unverified storage advanced durable publication: before=%+v after=%+v", before, after)
			}
			store.fail, store.beforeRead, store.afterStore = false, nil, nil
			for _, event := range events[test.sequence-1:] {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				stored := readReferencedRun(t, f, event.Data.AttemptID)
				if stored.Detail == nil || stored.Detail.ObjectKey == "" || stored.Sequence != event.Data.Sequence {
					t.Fatalf("restored storage lost readable reference: %+v", stored)
				}
			}
			after, uploads := read(), store.uploads
			if !after.present || after.sequence != 3 || after.status != "succeeded" || after.objects <= before.objects || after.history <= before.history {
				t.Fatalf("restored storage lost ordered run publication: before=%+v after=%+v", before, after)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
			if replay := read(); replay != after || store.uploads != uploads {
				t.Fatalf("replay changed restored publication: before=%+v after=%+v uploads=%d want=%d", after, replay, store.uploads, uploads)
			}
		})
	}
}

const runDetailReferenceCount = "SELECT count(*) FROM archive_objects o WHERE o.organization_id=$1 AND NOT EXISTS (SELECT 1 FROM archive_object_owners w WHERE w.organization_id=o.organization_id AND w.archive_id=o.id AND w.owner_kind='native-command')"

func TestNativeRunDetailAdmission(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		released       bool
		expired        bool
		sequence       int64
		cancelled      bool
		body           string
		status         int
		duringUpload   bool
		replay         bool
		batchGap       bool
		repeatedFinish bool
		reversed       bool
	}{
		{name: "repeated conflicting finish after ordered start", repeatedFinish: true, sequence: 1, status: http.StatusConflict},
		{name: "repeated finish with reversed usage interval", reversed: true, sequence: 2, status: http.StatusUnprocessableEntity},
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
			var initialReferences int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), runDetailReferenceCount, f.project.OrganizationID).Scan(&initialReferences); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			if test.repeatedFinish {
				plans := pilotHostedPlans()
				limited := plans.Plans[0]
				limited.ID, limited.Allowances = "run_detail_limit", maps.Clone(limited.Allowances)
				limited.Allowances["collaboration_bytes"] = 1
				plans.Plans, plans.Base = []HostedPlan{limited}, limited.PlanReference
				config := &HostedConfig{OrganizationID: string(f.project.OrganizationID), Plans: &plans, EntitlementAdministrator: "operator"}
				if err := f.service.database.configureHostedPlans(t.Context(), config); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					assignment, err := w.BillingAssignment(t.Context())
					if err != nil {
						return err
					}
					assignment.Base = billingstore.PlanReference{ID: limited.ID, Version: limited.Version}
					return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
				}); err != nil {
					t.Fatal(err)
				}
			}
			original := event
			if test.released {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET released_at=$1,release_reason='cancelled' WHERE organization_id=$2 AND lease_id=$3", now, f.project.OrganizationID, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.expired {
				now = lease.ExpiresAt
			}
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "next", test.sequence
			event.Data.Handoff = nativeTestCheckpoint()
			if test.cancelled {
				event.Type, event.Data.Outcome, event.Data.Handoff = "run.finished", "cancelled", nil
				event.Data.CompletionBody = test.body
			}
			if test.repeatedFinish {
				event.Type, event.Data.Outcome, event.Data.Handoff = "run.finished", "failed", nil
			}
			if test.reversed {
				event.Type, event.Data.Outcome, event.Data.Handoff = "run.finished", "succeeded", nil
				event.Data.Usage = []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 1, From: now.Add(-time.Minute), To: now.Add(-2 * time.Minute)}}
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
					now = lease.ExpiresAt
				}
			}
			repeats := 1
			if test.repeatedFinish || test.reversed {
				repeats = 4
			}
			initialUploads := store.uploads
			for i := range repeats {
				if test.repeatedFinish || test.reversed {
					event.IdempotencyKey = fmt.Sprintf("next-%d", i)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), test.status)
			}
			wantUploads := initialUploads
			if test.duringUpload {
				wantUploads += 3
			}
			if test.cancelled && test.status == http.StatusOK {
				wantUploads += 2
			}
			if store.uploads != wantUploads {
				t.Fatalf("rejected or identity-only event uploaded detail: %d uploads", store.uploads)
			}
			var references int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), runDetailReferenceCount, f.project.OrganizationID).Scan(&references); err != nil {
				t.Fatal(err)
			}
			wantReferences := 2
			if test.cancelled && test.status == http.StatusOK {
				wantReferences = 4
			}
			if references-initialReferences != wantReferences {
				t.Fatalf("event committed %d references, want %d", references-initialReferences, wantReferences)
			}
			row := readRunDetailAttemptRecord(t, f, event.Data.AttemptID)
			sequence, status := row.Sequence, row.Status
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

func TestNativeRunFirstFinishValidatesUsageBeforeUpload(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		usage  []tracker.NativeUsage
		status int
	}{
		{"reversed interval", []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 1, From: time.Date(2026, 10, 21, 11, 59, 0, 0, time.UTC), To: time.Date(2026, 10, 21, 11, 58, 0, 0, time.UTC)}}, http.StatusUnprocessableEntity},
		{"overlapping incremental sources", []tracker.NativeUsage{
			{Provider: "openai", Model: "example-model", UsageKind: "incremental", SourceID: "first", Revision: 1, Input: 1, From: time.Date(2026, 10, 21, 11, 57, 0, 0, time.UTC), To: time.Date(2026, 10, 21, 11, 59, 0, 0, time.UTC)},
			{Provider: "openai", Model: "example-model", UsageKind: "incremental", SourceID: "second", Revision: 1, Input: 1, From: time.Date(2026, 10, 21, 11, 58, 0, 0, time.UTC), To: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)},
		}, http.StatusUnprocessableEntity},
		{"valid interval", []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 1, From: time.Date(2026, 10, 21, 11, 58, 0, 0, time.UTC), To: time.Date(2026, 10, 21, 11, 59, 0, 0, time.UTC)}}, http.StatusOK},
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
			event.Type, event.Data.Outcome, event.Data.Identity = "run.finished", "failed", nil
			event.Data.TerminalFailure = &tracker.NativeTerminalFailure{ObservedAt: now, Decision: &tracker.FailureAttribution{Outcome: "instance", Reason: "recorded decision", Evidence: tracker.FailureEvidence{Step: "checkout"}}}
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1, HeartbeatAt: now, Phase: "completed"}
			event.Data.Usage = test.usage
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			repeats := 1
			if test.status != http.StatusOK {
				repeats = 3
			}
			initialUploads := store.uploads
			var initialObjects int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&initialObjects); err != nil {
				t.Fatal(err)
			}
			for i := range repeats {
				event.IdempotencyKey = fmt.Sprintf("first-finish-%d", i)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), test.status)
			}
			if test.status != http.StatusOK && store.uploads != initialUploads {
				t.Fatalf("rejected first finish uploaded %d payload objects", store.uploads-initialUploads)
			}
			if test.status != http.StatusOK {
				var objects int
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&objects); err != nil {
					t.Fatal(err)
				}
				if objects != initialObjects {
					t.Fatalf("rejected first finish registered %d payload objects", objects-initialObjects)
				}
			}
			if test.status == http.StatusOK && store.uploads == initialUploads {
				t.Fatal("accepted first finish uploaded no history")
			}
		})
	}
}

func TestNativeRunDetailIsolation(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: store})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other organization"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	seedPostgresHubAdmin(t, f.service.config.TenantRepository, f.service, organization)
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
				checkpoint.Data.ArtifactIDs = []string{newNativeID("artifact")}
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
				uploads := store.uploads
				store.beforeRead = func(context.Context) error {
					if held := fixture.service.database.postgres.Stats().AcquiredConns(); held != 0 {
						return fmt.Errorf("cancellation object read holds %d PostgreSQL connections", held)
					}
					return nil
				}
				store.afterStore = func(attachment.ArchiveReference) {
					if held := fixture.service.database.postgres.Stats().AcquiredConns(); held != 0 {
						t.Errorf("cancellation object upload holds %d PostgreSQL connections", held)
					}
				}
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled", CancelAttempt: true}), http.StatusNoContent)
				store.beforeRead, store.afterStore = nil, nil
				if store.uploads != uploads+2 {
					t.Fatalf("lease cancellation uploaded %d payloads, want 2", store.uploads-uploads)
				}
			} else {
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-"+session, 2, "failed"
				event.Data.CompletionBody = string(fixture.project.OrganizationID) + " " + session
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, fixture.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			stored := readReferencedRun(t, fixture, event.Data.AttemptID)
			if stored.Detail == nil {
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
				response := performHubAPIRequest(t, fixture.service, http.MethodGet, path+"/history?limit=100", worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var history tracker.Page[tracker.CollaborationEvent]
				decodeHubResponse(t, response, &history)
				var cancelled *tracker.NativeRunData
				for _, entry := range history.Items {
					if entry.Type == "run.finished" && entry.Data.Run != nil && entry.Data.Run.AttemptID == read.AttemptID {
						cancelled = entry.Data.Run
					}
				}
				if cancelled == nil {
					t.Fatal("history omitted lease cancellation")
				}
				if cancelled.Handoff != nil || len(cancelled.ArtifactIDs) != 0 {
					t.Error("cancellation history restored stale checkpoint or artifacts")
				}
				want := read.NativeRunData
				want.ArtifactIDs = nil
				if !reflect.DeepEqual(*cancelled, want) {
					t.Errorf("cancellation history differs from attempt: got %+v, want %+v", *cancelled, want)
				}
				row := readRunDetailAttemptRecord(t, fixture, event.Data.AttemptID)
				if row.Status != "cancelled" || stored.TerminalFailure == nil || stored.TerminalFailure.ErrorClass != "lease_released" {
					t.Fatalf("released typed state=%s failure=%+v", row.Status, stored.TerminalFailure)
				}
			}
			foreign := other
			if fixture.project.OrganizationID == other.project.OrganizationID {
				foreign = f
			}
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodGet, foreign.base+"/work-items/"+string(issue.WorkItemID)+"/attempts/"+event.Data.AttemptID, foreign.token, nil), http.StatusNotFound)
		}
		response := performHubAPIRequest(t, fixture.service, http.MethodGet, path+"/attempts?view=detail", worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.Page[tracker.NativeAttempt]
		decodeHubResponse(t, response, &page)
		if len(page.Items) != 2 || page.Items[0].AttemptID == page.Items[1].AttemptID || page.Items[0].CompletionBody == page.Items[1].CompletionBody {
			t.Fatal("both isolated attempts were not usable")
		}
	}
}

func TestNativeLeaseCancellationPreparation(t *testing.T) {
	t.Parallel()
	for _, human := range []bool{false, true} {
		for _, failure := range []string{"", "upload", "source changed"} {
			t.Run(fmt.Sprintf("human=%v/failure=%s", human, failure), func(t *testing.T) {
				store := &memoryRunDetails{}
				f := newDefaultNativeFixture(t, Config{RunDetails: store})
				if human {
					states := nativeFixtureStates()
					states[0].Transitions = append(states[0].Transitions, "Blocked")
					states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
					setChangeFixtureStates(t, f, states)
				}
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				issue := f.create(t, "cancellation archive")
				worker := f.worker(t, "worker")
				lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
				path := f.base + "/work-items/" + string(issue.WorkItemID)
				event := nativeStartedEvent(lease)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Handoff = "run.checkpointed", "checkpoint", 2, nativeTestCheckpoint()
				event.Data.ArtifactIDs = []string{newNativeID("artifact")}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
				if failure == "upload" {
					store.fail = true
				}
				if failure == "source changed" {
					store.afterStore = func(attachment.ArchiveReference) {
						store.afterStore = nil
						event.IdempotencyKey, event.Data.Sequence = "concurrent-checkpoint", 3
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
					}
				}
				status := http.StatusNoContent
				if human {
					status = http.StatusOK
				}
				switch failure {
				case "upload":
					status = http.StatusServiceUnavailable
				case "source changed":
					status = http.StatusConflict
				}
				if human {
					request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "operator-hold"}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "user_requested"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, request), status)
				} else {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled", CancelAttempt: true}), status)
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+event.Data.AttemptID, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var attempt tracker.NativeAttempt
				decodeHubResponse(t, response, &attempt)
				if failure != "" {
					if attempt.Status != "running" || attempt.Sequence != event.Data.Sequence || !attempt.Current {
						t.Fatalf("failed cancellation changed execution authority: %+v", attempt)
					}
				}
			})
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
	maintenance := (hubmaintenance.Budget{Items: 1, Bytes: BoardMaintenanceBytes}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(f.project.OrganizationID), Generation: 1})
	if _, processed, err := f.service.MaintainBoardRecord(maintenance, f.project.OrganizationID, ""); err != nil || !processed {
		t.Fatalf("background refresh did not process referenced run: processed=%v error=%v", processed, err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.BoardItem(t.Context(), f.project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.Display.Execution == nil || !row.Display.Execution.Running || row.Display.Execution.ID != event.Data.AttemptID {
			t.Fatalf("background refresh lost running attempt: %+v", row.Display.Execution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var dirty int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("background refresh left dirty referenced run: dirty=%d error=%v", dirty, err)
	}
}

type runtimeHistoryDetailStore struct {
	attachment.Storage
	mu         sync.Mutex
	objects    map[string][]byte
	reads      map[string]int
	beforeRead func() error
}

func (s *runtimeHistoryDetailStore) StoreDetail(_ context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ref.Key] = compressed
	return ref, nil
}

func (s *runtimeHistoryDetailStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.beforeRead != nil {
		if err := s.beforeRead(); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[key]++
	raw, ok := s.objects[key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s *runtimeHistoryDetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return attachment.ReadArchive(ctx, s, organization, ref)
}

func (s *runtimeHistoryDetailStore) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	object, err := s.Open(ctx, key)
	if err != nil {
		return nil, errors.Join(artifact.ErrStorage, err)
	}
	compressed, err := io.ReadAll(io.LimitReader(object, attachment.MaxArchiveBytes+1))
	if err = errors.Join(err, object.Close()); err != nil {
		return nil, errors.Join(artifact.ErrStorage, err)
	}
	return attachment.ReadCanonicalArchive(ctx, attemptArchiveStorage{data: compressed}, organization, project, key)
}

type runtimeHistoryAttempt struct {
	attempt    leasestore.Attempt
	event      eventstore.Event
	payload    attachment.ArchiveReference
	checkpoint attachment.ArchiveReference
}

func seedRuntimeHistoryAttempt(t *testing.T, repository tenantstore.Repository, store attachment.DetailStore, scope nativeScope, issue tracker.NativeIssue, runner runnerFixture, index int, preserves bool) runtimeHistoryAttempt {
	t.Helper()
	ctx := t.Context()
	descriptor := hubTestPolicy()
	var lease leasestore.Lease
	err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(ctx, scope.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(runner.binding.MachineID),
			SessionID: newNativeID("session"), LeaseID: newNativeID("lease"), TTL: time.Minute})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: scope.organization, RunnerID: runner.binding.RunnerID, LeaseID: lease.LeaseID, AssignedAt: lease.AcquiredAt}); err != nil {
			return err
		}
		return w.BindLeasePolicy(ctx, policystore.LeasePolicy{OrganizationID: scope.organization, ProjectID: scope.project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: lease.AcquiredAt})
	})
	if err != nil {
		t.Fatalf("seed history lease: %v", err)
	}

	checkpoint := nativeTestCheckpoint()
	checkpoint.WorktreeState, checkpoint.Resume = "clean", "fresh_checkout"
	if preserves {
		checkpoint.WorktreeState = "dirty"
	}
	data := nativeStartedEvent(tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: descriptor.ID}).Data
	data.MachineID, data.SessionID, data.RunnerID = runner.binding.MachineID, lease.SessionID, runner.binding.RunnerID
	data.Handoff, data.Outcome = checkpoint, "failed"
	data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "example failure"}
	data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", LocalAttemptID: int64(index + 1), Activity: &workflowmetrics.ActivityProfile{SessionID: 17, ProviderThreadRef: "thread-example"}}
	storeJSON := func(value any) attachment.ArchiveReference {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := store.StoreDetail(ctx, string(scope.organization), raw)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	payload := storeJSON(data)
	checkpointRef := storeJSON(checkpointPayload{Checkpoint: checkpoint})
	rawEvent, err := json.Marshal(tracker.CollaborationEvent{
		ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issue.WorkItemID,
		Type: "run.finished", SchemaVersion: 1, RecordedAt: lease.AcquiredAt, Actor: scope.actor(),
		Data: tracker.CollaborationData{Run: &data, Revision: tracker.Revision(lease.WorkItemRevision)},
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(store).PrepareEvent(ctx, scope.organization, rawEvent)
	if err != nil {
		t.Fatal(err)
	}
	fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
	var attempt leasestore.Attempt
	err = repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(ctx, leasestore.AttemptEvent{
			ID: data.AttemptID, RunID: data.RunID, Fence: fence, Sequence: data.Sequence, Status: "failed",
			StartedAt: lease.AcquiredAt, LocalAttemptID: data.Runtime.LocalAttemptID, PayloadObjectKey: payload.Key, PayloadReference: payload,
			CheckpointObjectKey: &checkpointRef.Key, CheckpointReference: checkpointRef, CheckpointPreservesSource: new(checkpoint.PreservesSource()),
		})
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		_, err = w.Release(ctx, fence, "finished")
		return err
	})
	if err != nil {
		t.Fatalf("seed history attempt: %v", err)
	}
	return runtimeHistoryAttempt{attempt: attempt, event: event, payload: payload, checkpoint: checkpointRef}
}

func TestNativeRuntimeRunDetailHistoryCost(t *testing.T) {
	t.Parallel()
	for _, history := range []int{1, 8, 64, 512} {
		for _, preserves := range []bool{false, true} {
			t.Run(strconv.Itoa(history)+"/preserves="+strconv.FormatBool(preserves), func(t *testing.T) {
				store := &runtimeHistoryDetailStore{objects: map[string][]byte{}, reads: map[string]int{}}
				repository, _ := projectRecordPostgresWithAdmin(t, 3, installReadQueryCounter)
				service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: store, SecretKeys: secretTestKeys(t, "1", "1")})
				organization := nativeOrganization{ID: "org_first", Name: "Example organization", Slug: "example-first"}
				seedPostgresHubAdmin(t, repository, service, organization)
				service.localOrganization = string(organization.ID)
				service.database.hostedOrganization = organization.ID
				service.requestMetrics.organization = organization.ID
				f := newNativeFixture(t, service, organization.ID, "checkpoint history")
				issue := f.create(t, "checkpoint history")
				registered := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				registered.enroll(t)
				credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, apikey.HashToken(f.token), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("fixture credential status=%d: %v", status, err)
				}
				scope := nativeScope{organization: organization.ID, project: f.project.ID, credential: credential}
				var first string
				var latest runtimeHistoryAttempt
				for i := range history {
					latest = seedRuntimeHistoryAttempt(t, repository, store, scope, issue, registered, i, preserves && i == 0)
					if i == 0 {
						first = latest.attempt.ID
					}
				}
				store.beforeRead = func() error {
					if held := repository.Stats().AcquiredConns(); held != 0 {
						return fmt.Errorf("run detail I/O holds %d PostgreSQL connections", held)
					}
					return nil
				}
				service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
					return func(c echo.Context) error {
						err := next(c)
						if strings.HasSuffix(c.Path(), "/explanation") {
							service.recordHostedRequest(c, time.Now())
						}
						return err
					}
				})
				wantReads := 1
				if preserves && history > 1 {
					wantReads = 2
				}
				base := f.base + "/work-items/" + string(issue.WorkItemID)
				if history == 1 {
					runtimePath := base + "/runtime?native_attempt_id=" + latest.attempt.ID
					for _, object := range []struct {
						name string
						ref  attachment.ArchiveReference
					}{{"run", latest.payload}, {"checkpoint", latest.checkpoint}} {
						t.Run(object.name+" archive", func(t *testing.T) {
							for _, availability := range []string{"available", "unavailable", "invalid"} {
								t.Run(availability, func(t *testing.T) {
									store.mu.Lock()
									original := store.objects[object.ref.Key]
									if len(original) == 0 {
										store.mu.Unlock()
										t.Fatal("fixture did not store the registered archive object")
									}
									if availability == "unavailable" {
										delete(store.objects, object.ref.Key)
									} else if availability == "invalid" {
										store.objects[object.ref.Key] = bytes.Repeat([]byte("!"), len(original))
									}
									store.mu.Unlock()
									t.Cleanup(func() {
										store.mu.Lock()
										store.objects[object.ref.Key] = original
										store.mu.Unlock()
									})
									response := performHubAPIRequest(t, service, http.MethodGet, runtimePath, f.token, nil)
									if availability == "available" {
										requireNativeStatus(t, response, http.StatusOK)
										return
									}
									requireNativeStatus(t, response, http.StatusServiceUnavailable)
									var failure struct {
										Code    string                    `json:"code"`
										Archive tracker.ArchiveDescriptor `json:"archive"`
									}
									decodeHubResponse(t, response, &failure)
									if failure.Code != "archive_"+availability || failure.Archive.Availability != availability || failure.Archive.SHA256 != object.ref.SHA256 || failure.Archive.SectionIDs == nil || len(failure.Archive.SectionIDs) != 0 {
										t.Fatalf("archive failure lost its classification: %s", response.Body.String())
									}
								})
							}
						})
					}
					t.Run("archive cancellation and canonical fallback", func(t *testing.T) {
						snapshot := tenantAttemptSnapshot{attempt: latest.attempt, event: latest.event, payload: &latest.payload}
						ctx, cancel := context.WithCancel(t.Context())
						cancel()
						var archive *nativeArchiveError
						if _, err := service.readNativeAttemptRunTenant(ctx, snapshot); !errors.Is(err, context.Canceled) || errors.As(err, &archive) {
							t.Fatalf("archive cancellation = %v", err)
						}
						store.mu.Lock()
						original := store.objects[latest.payload.Key]
						delete(store.objects, latest.payload.Key)
						store.mu.Unlock()
						t.Cleanup(func() {
							store.mu.Lock()
							store.objects[latest.payload.Key] = original
							store.mu.Unlock()
						})
						snapshot.payload, snapshot.recoverPayload = nil, true
						_, err := service.readNativeAttemptRunTenant(t.Context(), snapshot)
						if !errors.Is(err, artifact.ErrIntegrity) || !errors.Is(err, artifact.ErrStorage) || !errors.Is(err, artifact.ErrMissing) || !errors.As(err, &archive) {
							t.Fatalf("canonical fallback error = %v", err)
						}
						if archive.Archive.Availability != "unavailable" || archive.Archive.SHA256 != "" || archive.Archive.SectionIDs == nil || len(archive.Archive.SectionIDs) != 0 {
							t.Fatalf("canonical fallback archive = %+v", archive.Archive)
						}
						response := httptest.NewRecorder()
						request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, runtimePath, nil)
						if err := service.nativeAPIError(echo.New().NewContext(request, response), err); err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, response, http.StatusServiceUnavailable)
						if !strings.Contains(response.Body.String(), "archive_unavailable") || strings.Contains(response.Body.String(), "sha256") {
							t.Fatalf("canonical fallback response = %s", response.Body.String())
						}
					})
				}
				for _, endpoint := range []string{"runtime", "explanation"} {
					t.Run(endpoint, func(t *testing.T) {
						store.mu.Lock()
						clear(store.reads)
						store.mu.Unlock()
						var queries, writeTransactions, directWrites atomic.Int64
						timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
							queries.Add(1)
							query = strings.TrimSpace(query)
							for strings.HasPrefix(query, "--") {
								_, query, _ = strings.Cut(query, "\n")
								query = strings.TrimSpace(query)
							}
							query = strings.TrimSuffix(strings.ToUpper(strings.Join(strings.Fields(query), " ")), ";")
							verb, _, _ := strings.Cut(query, " ")
							switch verb {
							case "BEGIN":
								if !strings.Contains(query+" ", " READ ONLY ") {
									writeTransactions.Add(1)
								}
							case "INSERT", "UPDATE", "DELETE", "MERGE":
								directWrites.Add(1)
							}
						}}
						ctx := context.WithValue(t.Context(), entryKeyBoardTraceContext{}, timing)
						request := newHubAPIRequest(t, http.MethodGet, base+"/"+endpoint, f.token, nil).WithContext(ctx)
						response := httptest.NewRecorder()
						started := time.Now()
						service.Handler().ServeHTTP(response, request)
						requireNativeStatus(t, response, http.StatusOK)
						store.mu.Lock()
						var reads int
						for _, count := range store.reads {
							reads += count
						}
						payloadReads := store.reads[latest.payload.Key]
						store.mu.Unlock()
						t.Logf("%s history=%d preserves=%v reads=%d modeled_serial_latency=%s", endpoint, history, preserves, reads, time.Duration(reads)*250*time.Millisecond)
						expectedReads, expectedPayloadReads := wantReads, 1
						if endpoint == "explanation" {
							expectedReads, expectedPayloadReads = 0, 0
							var result explain.IssueExplanation
							decodeHubResponse(t, response, &result)
							if result.Attempt == nil || result.Attempt.NativeID != latest.attempt.ID || !result.Found || result.Identity.IssueID != string(issue.WorkItemID) {
								t.Error("explanation identity or latest attempt lost")
							}
							if result.Sessions.Detent == nil || result.Sessions.Detent.ID != "17" || result.Sessions.Provider == nil || result.Sessions.Provider.ID != "thread-example" {
								t.Errorf("session identities lost: %+v", result.Sessions)
							}
							_, failures, partial, err := service.readOperatorExplanation(context.WithValue(ctx, explanationReadKey{}, true), scope, issue)
							signature := latestNativeFailureSignature(failures, partial)
							if err != nil || signature == nil || signature.Signature != "example failure" || signature.Count != min(history, 64) || signature.Partial != (history > 64) {
								t.Errorf("bounded failure signature: %+v error=%v", signature, err)
							}
							if writeTransactions.Load() != 0 || directWrites.Load() != 0 {
								t.Errorf("explanation opened %d write transactions and issued %d direct write statements", writeTransactions.Load(), directWrites.Load())
							}
							if queries.Load() == 0 || queries.Load() > 180 || response.Body.Len() > 64*1024 {
								t.Errorf("unbounded or unobserved explanation: queries=%d bytes=%d", queries.Load(), response.Body.Len())
							}
							t.Logf("explanation history=%d queries=%d bytes=%d elapsed=%s writer_txs=%d direct_writes=%d", history, queries.Load(), response.Body.Len(), time.Since(started), writeTransactions.Load(), directWrites.Load())
						} else {
							var evidence tracker.NativeRuntimeEvidence
							decodeHubResponse(t, response, &evidence)
							if evidence.Attempt == nil || evidence.Attempt.AttemptID != latest.attempt.ID {
								t.Error("latest attempt lost")
							}
							if preserves && (evidence.SourceRecovery == nil || evidence.SourceRecovery.AttemptID != first) {
								t.Error("older preserved source lost")
							}
							if !preserves && evidence.SourceRecovery != nil {
								t.Error("clean checkpoint became preserved source")
							}
						}
						if reads != expectedReads || payloadReads != expectedPayloadReads {
							t.Errorf("object reads grew with checkpoint history: reads=%d want=%d registered payload=%d want=%d", reads, expectedReads, payloadReads, expectedPayloadReads)
						}
					})
				}
			})
		}
	}
}

func TestRunDetailSegmentResolvesPreparedReferenceBeforeCommit(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	organization := "org_prepared"
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	prepared := map[string]tracker.NativeRunData{"attempt_prepared:2": {AttemptID: "attempt_prepared", Sequence: 2}}
	owner := &runDetailContext{store: store, organization: organization, cache: map[string]map[string]tracker.NativeRunData{}, reference: &ref, runs: prepared}
	ctx := context.WithValue(t.Context(), runDetailContextKey{}, owner)
	runs, err := readRunDetailSegment(ctx, nil, newRunDetailReference(ref, organization, "attempt_prepared:2"))
	if err != nil || runs["attempt_prepared:2"].Sequence != 2 {
		t.Fatalf("prepared detail before commit = %v, %v", runs, err)
	}
}

func TestRunDetailSegmentRejectsLegacyArchiveWithoutDatabaseReader(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	organization := "org_archive"
	owner := &runDetailContext{store: store, organization: organization, cache: map[string]map[string]tracker.NativeRunData{}}
	ctx := context.WithValue(t.Context(), runDetailContextKey{}, owner)
	detail := &runDetailReference{AttachmentID: "att_legacy", Organization: organization, Entry: "attempt_legacy:1"}
	if _, err := readRunDetailSegment(ctx, nil, detail); !errors.Is(err, errRunDetailStoreUnavailable) {
		t.Fatalf("legacy archive without typed owner error = %v", err)
	}
}

func importRunDetailHistory(t *testing.T, f nativeFixture, issue tracker.NativeIssue, row leasestore.Attempt, run tracker.NativeRunData) {
	t.Helper()
	objects := f.service.runDetailStore()
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(checkpointPayload{Checkpoint: nativeTestCheckpoint()})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "issues", "leases", "machines", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "api_tokens", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "native_attempts", "native_attempt_events", "work_events", "change_content_objects", "attachments", "attempt_diffs", "attempt_diff_files", "attempt_diff_file_archives")
	fixture.add("issues", map[string]any{"id": row.IssueID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "native_id": issue.WorkItemID})
	fixture.add("native_attempts", map[string]any{"id": row.ID, "organization_id": row.OrganizationID, "project_id": f.project.ID, "work_item_id": issue.WorkItemID, "lease_id": row.LeaseID, "fencing_token": row.FencingToken, "run_id": row.RunID, "sequence": row.Sequence, "status": row.Status, "work_item_revision": row.WorkItemRevision, "dispatch_generation": row.DispatchGeneration, "data_json": string(payload), "checkpoint_json": string(checkpoint), "artifact_ids_json": "[]", "started_at": formatHubTime(row.StartedAt), "updated_at": formatHubTime(row.UpdatedAt), "payload_object_key": "", "checkpoint_object_key": ""})
	source := CoreExecutionImportSource{BundlePath: fixture.write(), OrganizationID: f.project.OrganizationID, SourceObjects: objects, Objects: objects}
	for range 2 {
		count, err := ImportCoreExecution(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), source)
		if err != nil || count != 1 {
			t.Fatalf("verified historical attempt import=%d want=1 error=%v", count, err)
		}
	}
}

func importRunDetailHistoryIntoEmptyTarget(t *testing.T, issue tracker.NativeIssue, row leasestore.Attempt, lease tracker.NativeLease, run tracker.NativeRunData, history map[int64]tracker.NativeRunData, now time.Time) {
	t.Helper()
	store := &memoryRunDetails{}
	target := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
	repository := target.service.config.TenantRepository.(*postgres.Repository)
	scope := analyticsTestScope(t, target)
	if err := repository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.WorkItem(t.Context(), target.project.ID, issue.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("historical target item already exists or cannot be checked: %v", err)
		}
		if _, err := r.Attempt(t.Context(), run.AttemptID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("historical target attempt already exists or cannot be checked: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, target.service, target.base+"/policy", hubTestPolicy())
	issue.OrganizationID, issue.ProjectID = target.project.OrganizationID, target.project.ID
	issue.UpdatedAt, issue.LastActivityAt = row.UpdatedAt, row.UpdatedAt
	source := newCoreNeutralFixture(t, target.project.OrganizationID, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "leases", "machines", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "api_tokens", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "native_attempts", "native_attempt_events", "work_events", "change_content_objects", "attachments", "attempt_diffs", "attempt_diff_files", "attempt_diff_file_archives")
	source.add("workflow_states", map[string]any{"id": 1, "project_id": target.project.ID, "detent_state": issue.State})
	source.add("issues", map[string]any{"id": 41, "native_id": issue.WorkItemID, "project_id": target.project.ID, "number": issue.Number, "revision": issue.Revision, "event_sequence": 7, "title": issue.Title, "body": issue.Body, "issue_contract_json": string(historyTestJSON(t, issue.IssueContract)), "report_context_json": string(historyTestJSON(t, issue.ReportContext)), "provenance_json": string(historyTestJSON(t, issue.Provenance)), "actor_json": string(historyTestJSON(t, issue.Actor)), "archived": issue.Archived, "created_at": formatHubTime(issue.CreatedAt), "updated_at": formatHubTime(issue.UpdatedAt), "native_created_at": formatHubTime(issue.CreatedAt), "native_updated_at": formatHubTime(issue.UpdatedAt), "last_activity_at": formatHubTime(issue.LastActivityAt), "workflow_state_id": 1, "labels_json": string(historyTestJSON(t, issue.Labels)), "assignees_json": string(historyTestJSON(t, issue.Assignees))})
	source.add("queue_entries", map[string]any{"id": 41, "issue_id": 41, "scope": "native", "state": issue.State, "rank": "010", "created_at": formatHubTime(issue.CreatedAt), "updated_at": formatHubTime(issue.UpdatedAt)})
	source.add("collaboration_versions", map[string]any{"project_id": target.project.ID, "work_item_id": issue.WorkItemID, "record_id": issue.WorkItemID, "revision": issue.Revision, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
	if len(history) != 7 {
		t.Fatalf("historical source has %d events, want7", len(history))
	}
	for sequence := int64(1); sequence <= 7; sequence++ {
		data, ok := history[sequence]
		if !ok {
			t.Fatalf("historical source missing sequence%d", sequence)
		}
		eventType := "run.checkpointed"
		switch sequence {
		case 1:
			eventType = "run.started"
		case 3, 4:
			eventType = "run.observed"
		case 7:
			eventType = "run.finished"
		}
		source.add("collaboration_events", map[string]any{"id": fmt.Sprintf("event_imported_run_%d", sequence), "project_id": target.project.ID, "work_item_id": issue.WorkItemID, "sequence": sequence, "type": eventType, "schema_version": 1, "actor_json": string(historyTestJSON(t, issue.Actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: issue.Revision, Run: &data})), "event_revision": issue.Revision, "recorded_at": formatHubTime(row.StartedAt)})
	}
	execution := source
	execution.add("machines", map[string]any{"id": lease.MachineID, "hostname": "example-host", "display_name": "Example host", "capacity": 2, "version": "test", "sprite_name": "", "last_heartbeat_at": formatHubTime(lease.AcquiredAt), "registered_at": formatHubTime(lease.AcquiredAt), "updated_at": formatHubTime(row.UpdatedAt)})
	execution.add("leases", map[string]any{"fencing_token": lease.FencingToken, "lease_id": lease.ID, "issue_id": 41, "machine_id": lease.MachineID, "session_id": lease.SessionID, "work_item_revision": row.WorkItemRevision, "landing_capacity": 0, "acquired_at": formatHubTime(lease.AcquiredAt), "renewed_at": formatHubTime(lease.RenewedAt), "expires_at": formatHubTime(lease.ExpiresAt), "released_at": formatHubTime(row.UpdatedAt)})
	execution.add("lease_policies", map[string]any{"lease_id": lease.ID, "scope": string(target.project.OrganizationID) + "/" + string(target.project.ID), "policy_id": run.PolicyID})
	execution.add("native_attempts", map[string]any{"id": run.AttemptID, "project_id": target.project.ID, "work_item_id": issue.WorkItemID, "lease_id": run.LeaseID, "fencing_token": run.FencingToken, "run_id": run.RunID, "sequence": run.Sequence, "status": row.Status, "work_item_revision": row.WorkItemRevision, "dispatch_generation": row.DispatchGeneration, "data_json": string(historyTestJSON(t, run)), "checkpoint_json": string(historyTestJSON(t, checkpointPayload{Checkpoint: nativeTestCheckpoint()})), "artifact_ids_json": "[]", "started_at": formatHubTime(row.StartedAt), "updated_at": formatHubTime(row.UpdatedAt), "payload_object_key": "", "checkpoint_object_key": ""})
	workSource := CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: target.project.OrganizationID, ProjectID: target.project.ID, Objects: store}
	for range 2 {
		count, err := ImportCoreWorkItems(t.Context(), repository, workSource)
		if err != nil || count != 1 {
			t.Fatalf("historical work item import=%d want1 error=%v", count, err)
		}
	}
	executionSource := CoreExecutionImportSource{BundlePath: workSource.BundlePath, OrganizationID: target.project.OrganizationID, SourceObjects: store, Objects: store}
	for range 2 {
		count, err := ImportCoreExecution(t.Context(), repository, executionSource)
		if err != nil || count != 3 {
			t.Fatalf("historical execution import=%d want3 error=%v", count, err)
		}
	}
	snapshot, err := target.service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(issue.WorkItemID), run.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := target.service.readNativeAttemptRunTenant(t.Context(), snapshot)
	if err != nil || !reflect.DeepEqual(imported, run) {
		t.Fatalf("historical attempt payload differs from inline source: %v", err)
	}
	checkpoint, err := target.service.readNativeAttemptCheckpointTenant(t.Context(), snapshot)
	if err != nil || !reflect.DeepEqual(checkpoint, nativeTestCheckpoint()) {
		t.Fatalf("historical checkpoint differs from inline source: %v", err)
	}
	assertAttempt := func(name string, attempt *tracker.NativeAttempt, summary bool) {
		t.Helper()
		if attempt == nil {
			t.Fatalf("%s omitted historical attempt", name)
		}
		want := run
		if summary {
			want.Runtime = nil
		}
		if !reflect.DeepEqual(attempt.NativeRunData, want) || !reflect.DeepEqual(attempt.Checkpoint, nativeTestCheckpoint()) || attempt.Status != row.Status || !attempt.StartedAt.Equal(row.StartedAt) || !attempt.UpdatedAt.Equal(row.UpdatedAt) || attempt.WorkItemRevision != tracker.Revision(row.WorkItemRevision) || attempt.DispatchGeneration != row.DispatchGeneration {
			t.Fatalf("%s differs from independent inline attempt/checkpoint source", name)
		}
	}
	assertHistory := func(name string, page tracker.Page[tracker.CollaborationEvent]) {
		t.Helper()
		if len(page.Items) != 7 || page.NextCursor != "" {
			t.Fatalf("%s history count=%d continuation=%q want7complete", name, len(page.Items), page.NextCursor)
		}
		for index, event := range page.Items {
			sequence := int64(index + 1)
			if event.AggregateSequence != sequence || event.Data.Run == nil || !reflect.DeepEqual(*event.Data.Run, history[sequence]) || event.OrganizationID != target.project.OrganizationID || event.ProjectID != target.project.ID || event.AggregateID != issue.WorkItemID {
				t.Fatalf("%s historical event%d differs from independent inline source", name, sequence)
			}
		}
	}
	path := target.base + "/work-items/" + string(issue.WorkItemID)
	for _, suffix := range []string{"/attempts", "/attempts/" + run.AttemptID, "/runtime", "/history?limit=100"} {
		response := performHubAPIRequest(t, target.service, http.MethodGet, path+suffix, target.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		switch suffix {
		case "/attempts":
			var page tracker.Page[tracker.NativeAttempt]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.NextCursor != "" {
				t.Fatalf("historical attempts=%d continuation=%q", len(page.Items), page.NextCursor)
			}
			assertAttempt(suffix, &page.Items[0], false)
		case "/attempts/" + run.AttemptID:
			var attempt tracker.NativeAttempt
			decodeHubResponse(t, response, &attempt)
			assertAttempt(suffix, &attempt, false)
		case "/runtime":
			var runtime tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &runtime)
			assertAttempt(suffix, runtime.Attempt, false)
		default:
			var page tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &page)
			assertHistory(suffix, page)
		}
	}
	executor := hostedOperatorExecutor{service: target.service}
	ctx := changeOperatorContext(t, target.service, target.token, string(target.project.OrganizationID))
	for _, name := range []string{operatortool.WorkRuns, operatortool.WorkAttemptReceipt, operatortool.GetNativeRun, operatortool.WorkHistory} {
		args := map[string]any{"project_id": target.project.ID, "reference": issue.WorkItemID}
		if name == operatortool.WorkAttemptReceipt {
			args["native_attempt_id"] = run.AttemptID
		}
		if name == operatortool.GetNativeRun {
			args = map[string]any{"project_id": target.project.ID, "work_item_id": issue.WorkItemID, "attempt_id": run.AttemptID}
		}
		result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: historyTestJSON(t, args)})
		if err != nil {
			t.Fatalf("historical %s: %v", name, err)
		}
		switch name {
		case operatortool.WorkRuns:
			var view operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
			if err := json.Unmarshal(result.Content, &view); err != nil {
				t.Fatal(err)
			}
			if len(view.Data.Items) != 1 || view.Data.NextCursor != "" {
				t.Fatalf("historical work_runs=%d continuation=%q", len(view.Data.Items), view.Data.NextCursor)
			}
			assertAttempt(name, &view.Data.Items[0], true)
		case operatortool.WorkAttemptReceipt:
			var view operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
			if err := json.Unmarshal(result.Content, &view); err != nil {
				t.Fatal(err)
			}
			assertAttempt(name, view.Data.Attempt, false)
		case operatortool.GetNativeRun:
			var view operatortool.ChangeResult
			if err := json.Unmarshal(result.Content, &view); err != nil {
				t.Fatal(err)
			}
			assertAttempt(name, view.Attempt, false)
		case operatortool.WorkHistory:
			var view operatortool.WorkReadResult[tracker.Page[tracker.CollaborationEvent]]
			if err := json.Unmarshal(result.Content, &view); err != nil {
				t.Fatal(err)
			}
			assertHistory(name, view.Data)
		}
	}
}

func readRunDetailAttemptRecord(t *testing.T, f nativeFixture, id string) leasestore.Attempt {
	t.Helper()
	var row leasestore.Attempt
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Attempt(t.Context(), id)
		if err != nil {
			return err
		}
		_, err = r.WorkItemByID(t.Context(), f.project.ID, row.IssueID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func readReferencedRun(t *testing.T, f nativeFixture, attempt string) storedRun {
	t.Helper()
	row := readRunDetailAttemptRecord(t, f, attempt)
	var item corestore.WorkItem
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		item, err = r.WorkItemByID(t.Context(), f.project.ID, row.IssueID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), analyticsTestScope(t, f), string(item.NativeID), attempt)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.service.readNativeAttemptRunTenant(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.payload == nil {
		t.Fatal("attempt lost its registered payload reference")
	}
	detail := newRunDetailReference(*snapshot.payload, string(f.project.OrganizationID), runDetailEntry(run))
	detail.ObjectKey = row.PayloadObjectKey
	return storedRun{NativeRunData: run, Detail: detail}
}

func TestAttemptHydrationIntegrityStages(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		stage string
		field string
		edit  func(*tracker.CollaborationEvent, *tenantAttemptSnapshot, *memoryRunDetails)
	}{
		{name: "missing run", stage: "hydrate attempt run", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) { v.Data.Run = nil }},
		{name: "run id", stage: "hydrate attempt identity", field: "run_id", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) {
			v.Data.Run.RunID = "private-run"
		}},
		{name: "attempt id", stage: "hydrate attempt identity", field: "attempt_id", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) {
			v.Data.Run.AttemptID = "private-attempt"
		}},
		{name: "lease id", stage: "hydrate attempt identity", field: "lease_id", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) {
			v.Data.Run.LeaseID = "private-lease"
		}},
		{name: "fence", stage: "hydrate attempt identity", field: "fencing_token", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) {
			v.Data.Run.FencingToken++
		}},
		{name: "sequence", stage: "hydrate attempt identity", field: "sequence", edit: func(v *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, _ *memoryRunDetails) {
			v.Data.Run.Sequence++
		}},
		{name: "attachment evidence", stage: "hydrate attempt evidence", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.evidence = true
			s.evidenceRows = []attachmentstore.Record{{OrganizationID: "private-organization", EvidenceCaption: "private-caption"}}
		}},
		{name: "event identity", stage: "validate event identity", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.event.ID = "private-event"
		}},
		{name: "push projection", stage: "validate event push projection", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.event.PushKnown = true
			s.event.PushHeadSHA = "private-head"
		}},
		{name: "landing projection", stage: "validate event landing projection", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.event.LandingWaiting = true
		}},
		{name: "reference", stage: "validate history reference", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.event.PayloadObjectKey = "private-object-key"
		}},
		{name: "length", stage: "validate history content length", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, _ *memoryRunDetails) {
			s.event.PayloadContentBytes++
		}},
		{name: "decode", stage: "decode history payload", edit: func(_ *tracker.CollaborationEvent, s *tenantAttemptSnapshot, m *memoryRunDetails) {
			m.data[s.event.PayloadObjectKey] = bytes.Repeat([]byte("!"), int(s.event.PayloadContentBytes))
		}},
		{name: "read", stage: "read history payload", edit: func(_ *tracker.CollaborationEvent, _ *tenantAttemptSnapshot, m *memoryRunDetails) {
			m.beforeRead = func(context.Context) error { return &attachment.ArchiveReadError{Err: artifact.ErrIntegrity} }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryRunDetails{}
			service := &Service{config: Config{RunDetails: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			value := tracker.CollaborationEvent{ID: "event", OrganizationID: "org", ProjectID: "project", AggregateID: "item", Type: "run.updated", SchemaVersion: 1, Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt", RunID: "run", LeaseID: "lease", FencingToken: 7, Sequence: 2}}}
			snapshot := tenantAttemptSnapshot{attempt: leasestore.Attempt{OrganizationID: "org", ID: "attempt", RunID: "run", Sequence: 2}, lease: leasestore.Lease{LeaseID: "lease", FencingToken: 7}}
			content := eventstore.NewContent(store)
			prepare := func() eventstore.Event {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareEvent(t.Context(), "org", raw)
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			snapshot.event = prepare()
			if _, err := service.hydrateNativeAttemptTenant(t.Context(), snapshot, time.Now()); err != nil {
				t.Fatal(err)
			}
			test.edit(&value, &snapshot, store)
			if test.name == "missing run" || test.stage == "hydrate attempt identity" {
				snapshot.event = prepare()
			}
			got, err := service.hydrateNativeAttemptTenant(t.Context(), snapshot, time.Now())
			if !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("integrity identity lost: %v", err)
			}
			if !reflect.DeepEqual(got, tracker.NativeAttempt{}) {
				t.Fatal("integrity failure returned an attempt")
			}
			wantError := test.stage + ": " + artifact.ErrIntegrity.Error()
			if test.field != "" {
				wantError += ": " + test.field
			}
			if test.name == "read" {
				wantError += "\nread archive: " + artifact.ErrIntegrity.Error()
			}
			if err.Error() != wantError {
				t.Errorf("missing static stage or leaked raw values: %v", err)
			}
			if test.name == "read" {
				var readError *attachment.ArchiveReadError
				if !errors.As(err, &readError) {
					t.Fatal("archive read error type lost")
				}
			}
			response := func(failure error) *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				if err := service.nativeAPIError(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder), failure); err != nil {
					t.Fatal(err)
				}
				return recorder
			}
			before, after := response(artifact.ErrIntegrity), response(err)
			if after.Code != http.StatusInternalServerError || after.Code != before.Code || after.Body.String() != before.Body.String() {
				t.Fatalf("public response changed: %d %s", after.Code, after.Body.String())
			}
		})
	}
}
