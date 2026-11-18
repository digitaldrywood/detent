package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

type s3RunDetails struct{ attachment.ArchiveContents }

func (s s3RunDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	return s.PublishContent(ctx, organization, raw)
}
func (s s3RunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return s.ReadContent(ctx, organization, ref)
}

func pendingRunEvent(f *attemptDiffFixture, key string) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: key}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
}

func pendingEventPath(f *attemptDiffFixture) string {
	return f.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
}

func readPublishedAttempt(t *testing.T, f *attemptDiffFixture) tracker.NativeAttempt {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/attempts/"+f.attempt, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var attempt tracker.NativeAttempt
	decodeHubResponse(t, response, &attempt)
	return attempt
}

type pendingRunSnapshot struct{ sequence, charged, objects, commands, history int64 }

func readPendingRunSnapshot(t *testing.T, f *attemptDiffFixture) pendingRunSnapshot {
	t.Helper()
	var value pendingRunSnapshot
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), f.attempt)
		if err != nil {
			return err
		}
		value.sequence = attempt.Sequence
		value.charged, err = r.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM archive_objects WHERE organization_id=$1),(SELECT count(*) FROM native_commands WHERE organization_id=$1),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID).Scan(&value.objects, &value.commands, &value.history); err != nil {
		t.Fatal(err)
	}
	return value
}

func pendingCommitFailure(t *testing.T, f *attemptDiffFixture) {
	t.Helper()
	admin := usagePostgresAdmin(t, f.service)
	for _, statement := range []string{
		`CREATE FUNCTION example_publication_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.organization_id=TG_ARGV[0] THEN RAISE EXCEPTION 'example publication failure'; END IF; RETURN NEW; END$$`,
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER example_publication_failure AFTER INSERT ON native_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION example_publication_failure('%s')`, strings.ReplaceAll(string(f.project.OrganizationID), "'", "''")),
	} {
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeEventPendingPublicationS3(t *testing.T) {
	t.Parallel()
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "recovery"}[recovery], func(t *testing.T) {
			var mu sync.Mutex
			objects := make(map[string][]byte)
			unavailable, uploads := false, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") == "" || unavailable && r.Method == http.MethodPut {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
					return
				}
				switch r.Method {
				case http.MethodPut:
					data, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "read object", http.StatusBadRequest)
						return
					}
					objects[r.URL.Path] = data
					if !strings.Contains(r.URL.Path, "/probe/") {
						uploads++
					}
				case http.MethodDelete:
					delete(objects, r.URL.Path)
				case http.MethodGet, http.MethodHead:
					data, ok := objects[r.URL.Path]
					if !ok {
						if unavailable {
							w.WriteHeader(http.StatusForbidden)
							_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
							return
						}
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
						return
					}
					if r.Method == http.MethodGet {
						_, _ = w.Write(data)
					}
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(server.Close)
			storage, err := attachment.NewStorage(t.Context(), attachment.Config{
				Endpoint: server.URL, Region: "test", Bucket: "private",
				AccessKeyID: "fixture-access", SecretAccessKey: "fixture-secret",
			}, server.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: s3RunDetails{ArchiveContents: attachment.ArchiveContents{Storage: storage.(attachment.ArchiveStorage)}}})
			f.start(t)
			before := readPendingRunSnapshot(t, f)
			mu.Lock()
			unavailable = recovery
			beforeUploads := uploads
			mu.Unlock()
			event := pendingRunEvent(f, "s3-finish")
			response := performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event)
			if recovery {
				requireNativeStatus(t, response, http.StatusServiceUnavailable)
				if after := readPendingRunSnapshot(t, f); after != before {
					t.Fatalf("unavailable storage advanced metadata: before=%+v after=%+v", before, after)
				}
				mu.Lock()
				failedUploads := uploads
				unavailable = false
				mu.Unlock()
				if failedUploads != beforeUploads {
					t.Fatal("storage failure permitted an upload")
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event)
			}
			requireNativeStatus(t, response, http.StatusOK)
			after := readPendingRunSnapshot(t, f)
			if after.sequence != 2 || after.charged <= before.charged || after.objects != before.objects+2 {
				t.Fatalf("publication missing new run/history and shared receipt: before=%+v after=%+v", before, after)
			}
			mu.Lock()
			gotUploads := uploads
			mu.Unlock()
			if gotUploads != beforeUploads+2 {
				t.Fatalf("uploads=%d want=%d", gotUploads, beforeUploads+2)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			if replay := readPendingRunSnapshot(t, f); replay != after {
				t.Fatalf("replay changed publication: before=%+v after=%+v", after, replay)
			}
			mu.Lock()
			replayUploads := uploads
			mu.Unlock()
			if replayUploads != gotUploads {
				t.Fatal("replay uploaded stable content again")
			}
			if got := readPublishedAttempt(t, f); got.Sequence != 2 || got.Outcome != "succeeded" {
				t.Fatal("published payload lost committed run history")
			}
		})
	}
}

func TestChangeContentReadSurvivesPendingPublication(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store})
	f.start(t)
	uploaded, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var pause, release sync.Once
	store.afterStore = func(attachment.ArchiveReference) { pause.Do(func() { close(uploaded); <-resume }) }
	event := pendingRunEvent(f, "reader-publication")
	request := newHubAPIRequest(t, http.MethodPost, pendingEventPath(f), f.worker, event)
	response := httptest.NewRecorder()
	go func() { defer close(done); f.service.Handler().ServeHTTP(response, request) }()
	t.Cleanup(func() { release.Do(func() { close(resume) }); <-done })
	select {
	case <-uploaded:
	case <-done:
		t.Fatalf("request returned before preparation: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := readPublishedAttempt(t, f); got.Sequence != 1 || got.Outcome != "" {
		t.Fatalf("reader observed unpublished run: %+v", got)
	}
	release.Do(func() { close(resume) })
	<-done
	requireNativeStatus(t, response, http.StatusOK)
	if got := readPublishedAttempt(t, f); got.Sequence != 2 || got.Outcome != "succeeded" {
		t.Fatalf("reader lost committed content: %+v", got)
	}
}

type uncertainRunDetails struct {
	*memoryRunDetails
	uncertain, unreadable, rejected bool
	blocked                         map[string]bool
	writes                          int
}

func (s *uncertainRunDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.writes++
	if s.rejected {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	ref, err := s.memoryRunDetails.StoreDetail(ctx, organization, raw)
	if err != nil {
		return ref, err
	}
	if s.unreadable {
		if s.blocked == nil {
			s.blocked = make(map[string]bool)
		}
		s.blocked[ref.Key] = true
	}
	if s.uncertain {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	return ref, nil
}

func (s *uncertainRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.blocked[ref.Key] {
		return nil, artifact.ErrStorage
	}
	return s.memoryRunDetails.ReadDetail(ctx, organization, ref)
}

func TestNativeEventRejectionsLeaveNoContent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"lease", "fence", "authority", "sequence", "usage conflict", "allowance", "commit failure"} {
		t.Run(name, func(t *testing.T) {
			clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
			store := &memoryRunDetails{}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store, Clock: clock.Now})
			f.start(t)
			event := pendingRunEvent(f, "rejected-event")
			status := http.StatusConflict
			admin := usagePostgresAdmin(t, f.service)
			switch name {
			case "lease":
				result, err := admin.ExecContext(t.Context(), `UPDATE leases SET expires_at=renewed_at+interval '1 microsecond' WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, f.lease.ID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("lease expiry rows=%d error=%v", n, err)
				}
				clock.Advance(2 * time.Microsecond)
			case "fence":
				event.Data.FencingToken++
			case "authority":
				credential, code, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.worker), "", "")
				if err != nil || code != http.StatusOK {
					t.Fatalf("worker authority=%d error=%v", code, err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessCredential(t.Context(), credential.ID, clock.Now())
				}); err != nil {
					t.Fatal(err)
				}
				status = http.StatusUnauthorized
			case "sequence":
				result, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempt_events(organization_id,attempt_id,sequence,request_hash) VALUES($1,$2,2,$3)`, f.project.OrganizationID, f.attempt, strings.Repeat("a", 64))
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("sequence rows=%d error=%v", n, err)
				}
			case "usage conflict":
				start := clock.Now()
				clock.Advance(time.Second)
				now := clock.Now()
				event.OccurredAt = now
				event.Data.Usage = []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 1, Revision: 1, From: start, To: now}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
				event.IdempotencyKey, event.Data.Sequence, event.Data.Usage[0].Input = "conflicting-usage", 3, 2
			case "allowance":
				ref := billingstore.PlanReference{ID: "example-run-content", Version: 1}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: f.project.OrganizationID, PlanReference: ref, Name: "Example run content", Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 1}}); err != nil {
						return err
					}
					assignment, err := w.BillingAssignment(t.Context())
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: f.project.OrganizationID, Base: ref}, assignment.Revision)
				}); err != nil {
					t.Fatal(err)
				}
				f.service.database.billingRepository = f.service.config.TenantRepository
				f.service.config.BillingRepository = f.service.config.TenantRepository
				f.service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{Base: hostedplans.PlanReference{ID: ref.ID, Version: ref.Version}, WindowSeconds: 60, RetentionWindows: 10}})
				event.Type, event.Data.Outcome = "run.observed", ""
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: clock.Now()}
				status = http.StatusTooManyRequests
			case "commit failure":
				pendingCommitFailure(t, f)
				status = http.StatusInternalServerError
			}
			before := readPendingRunSnapshot(t, f)
			store.mu.Lock()
			uploads := store.uploads
			store.mu.Unlock()
			response := performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event)
			requireNativeStatus(t, response, status)
			if after := readPendingRunSnapshot(t, f); after != before {
				t.Fatalf("rejection changed committed content: before=%+v after=%+v", before, after)
			}
			store.mu.Lock()
			afterUploads := store.uploads
			store.mu.Unlock()
			if name != "allowance" && name != "commit failure" && afterUploads != uploads {
				t.Fatalf("preparation preceded definite rejection: uploads=%d want=%d", afterUploads, uploads)
			}
			if name == "commit failure" && !strings.Contains(response.Body.String(), "native_operation_failed") {
				t.Fatalf("unexpected commit rejection: %s", response.Body.String())
			}
		})
	}
}

type usageRaceDetails struct {
	attachment.DetailStore
	mu       sync.Mutex
	armed    bool
	keys     []string
	raceOnce func([]byte)
}

func (d *usageRaceDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := d.DetailStore.StoreDetail(ctx, organization, raw)
	d.mu.Lock()
	race := d.armed && err == nil && bytes.Contains(raw, []byte(`"source_id"`))
	if d.armed && err == nil {
		d.keys = append(d.keys, ref.Key)
	}
	if race {
		d.armed = false
	}
	d.mu.Unlock()
	if race {
		d.raceOnce(raw)
	}
	return ref, err
}

func TestNativeUsageConflictDuringPublicationLeavesOnlyReclaimableObjects(t *testing.T) {
	t.Parallel()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	f := claimedAttemptDiffFixture(t, false, Config{Clock: clock.Now})
	objects := f.service.config.TenantRepository.(*postgres.Repository).DetailStore(clock.Now)
	details := &usageRaceDetails{DetailStore: objects}
	f.service.config.RunDetails = details
	f.start(t)
	start := clock.Now()
	clock.Advance(time.Second)
	loser := pendingRunEvent(f, "usage-loser")
	loser.OccurredAt = clock.Now()
	loser.Data.Usage = []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 2, Revision: 1, From: start, To: clock.Now()}}
	var conflictKey string
	details.raceOnce = func(raw []byte) {
		var observation usagecost.Observation
		if err := json.Unmarshal(raw, &observation); err != nil {
			t.Error(err)
			return
		}
		observation.Input++
		conflicting, err := json.Marshal(observation)
		if err != nil {
			t.Error(err)
			return
		}
		ref, err := objects.StoreDetail(t.Context(), string(f.project.OrganizationID), conflicting)
		if err != nil {
			t.Error(err)
			return
		}
		conflictKey = ref.Key
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordBillingCosts(t.Context(), []billingstore.CostObservation{{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: clock.Now()}})
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	details.armed = true
	before := readPendingRunSnapshot(t, f)
	response := performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, loser)
	if conflictKey == "" {
		t.Fatal("conflicting usage was not committed during the losing publication")
	}
	requireNativeCode(t, response, http.StatusConflict, "idempotency_conflict")
	details.mu.Lock()
	losingKeys := append([]string(nil), details.keys...)
	details.mu.Unlock()
	if len(losingKeys) == 0 {
		t.Fatal("losing publication uploaded no objects before its write")
	}
	var events, receipts, revisions int
	admin := usagePostgresAdmin(t, f.service)
	if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND type LIKE 'run.%' AND payload_object_key=ANY($2::text[])),(SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$3),(SELECT count(*) FROM cost_observation_revisions WHERE organization_id=$1 AND object_key=ANY($2::text[]))", f.project.OrganizationID, losingKeys, loser.IdempotencyKey).Scan(&events, &receipts, &revisions); err != nil {
		t.Fatal(err)
	}
	if events != 0 || receipts != 0 || revisions != 0 {
		t.Fatalf("losing publication committed events=%d receipts=%d usage revisions=%d", events, receipts, revisions)
	}
	if after := readPendingRunSnapshot(t, f); after != before {
		t.Fatalf("losing publication changed committed content: before=%+v after=%+v", before, after)
	}
	var winningKeys []string
	rows, err := admin.QueryContext(t.Context(), "SELECT object_key FROM cost_observation_revisions WHERE organization_id=$1", f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		winningKeys = append(winningKeys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(winningKeys) == 0 {
		t.Fatalf("winning usage revision keys=%v err=%v", winningKeys, err)
	}
	clock.Advance(attachment.OrphanTTL + time.Second)
	if _, err := objects.Reclaim(t.Context(), f.project.OrganizationID, clock.Now().Add(-attachment.OrphanTTL)); err != nil {
		t.Fatal(err)
	}
	reclaimed := 0
	for _, key := range losingKeys {
		var present bool
		if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2)", f.project.OrganizationID, key).Scan(&present); err != nil {
			t.Fatal(err)
		}
		var referenced bool
		if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=$1 AND payload_object_key=$2) OR EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=$1 AND payload_object_key=$2) OR EXISTS(SELECT 1 FROM archive_objects WHERE organization_id=$1 AND object_key=$2) OR EXISTS(SELECT 1 FROM cost_observation_revisions WHERE organization_id=$1 AND object_key=$2)", f.project.OrganizationID, key).Scan(&referenced); err != nil {
			t.Fatal(err)
		}
		if present != referenced {
			t.Fatalf("object %s present=%t after reclaim, want %t (referenced by committed work)", key, present, referenced)
		}
		if !present {
			reclaimed++
		}
	}
	if reclaimed == 0 {
		t.Fatal("reclaim left every object of the rejected publication")
	}
	for _, key := range winningKeys {
		var present bool
		if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2)", f.project.OrganizationID, key).Scan(&present); err != nil || !present {
			t.Fatalf("reclaim deleted committed usage object %s: present=%t err=%v", key, present, err)
		}
	}
}

func TestNativeEventUploadsOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"fencing generation", "owner", "lease expiry", "lease release"} {
		t.Run(name, func(t *testing.T) {
			clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
			store := &memoryRunDetails{}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store, Clock: clock.Now})
			f.start(t)
			before := readPendingRunSnapshot(t, f)
			uploaded, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var pause, release sync.Once
			store.afterStore = func(attachment.ArchiveReference) {
				if acquired := f.service.database.postgres.Stats().AcquiredConns(); acquired != 0 {
					t.Errorf("object preparation held %d PostgreSQL connections", acquired)
				}
				pause.Do(func() { close(uploaded); <-resume })
			}
			event := pendingRunEvent(f, "committed-finish")
			request := newHubAPIRequest(t, http.MethodPost, pendingEventPath(f), f.worker, event)
			response := httptest.NewRecorder()
			go func() { defer close(done); f.service.Handler().ServeHTTP(response, request) }()
			t.Cleanup(func() { release.Do(func() { close(resume) }); <-done })
			select {
			case <-uploaded:
			case <-done:
				t.Fatalf("request returned before upload: status=%d body=%s", response.Code, response.Body.String())
			}
			if after := readPendingRunSnapshot(t, f); after != before {
				t.Fatalf("preparation advanced metadata: before=%+v after=%+v", before, after)
			}
			admin := usagePostgresAdmin(t, f.service)
			status := http.StatusConflict
			switch name {
			case "fencing generation":
				_, err := admin.ExecContext(t.Context(), `UPDATE leases SET fencing_token=fencing_token+1 WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, f.lease.ID)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "428C9" {
					t.Fatalf("admitted lease fence mutation=%v want generated identity refusal", err)
				}
				status = http.StatusOK
			case "owner":
				credential, code, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.worker), "", "")
				if err != nil || code != http.StatusOK {
					t.Fatalf("worker authority=%d error=%v", code, err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessCredential(t.Context(), credential.ID, clock.Now())
				}); err != nil {
					t.Fatal(err)
				}
				status = http.StatusForbidden
			case "lease expiry":
				result, err := admin.ExecContext(t.Context(), `UPDATE leases SET expires_at=renewed_at+interval '1 microsecond' WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, f.lease.ID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("expiry rows=%d error=%v", n, err)
				}
				clock.Advance(2 * time.Microsecond)
			case "lease release":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(f.lease.ID), FencingToken: int64(f.lease.FencingToken)}, "example release")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			release.Do(func() { close(resume) })
			<-done
			requireNativeStatus(t, response, status)
			after := readPendingRunSnapshot(t, f)
			if status != http.StatusOK {
				if after != before {
					t.Fatalf("superseded preparation committed content: before=%+v after=%+v", before, after)
				}
				if got := readPublishedAttempt(t, f); got.Sequence != 1 || got.Outcome != "" {
					t.Fatal("rejected preparation changed committed run history")
				}
			} else {
				if after.sequence != 2 {
					t.Fatalf("immutable fence prevented valid publication: %+v", after)
				}
				if got := readPublishedAttempt(t, f); got.Sequence != 2 || got.Outcome != "succeeded" {
					t.Fatal("immutable fence lost committed run history")
				}
				store.mu.Lock()
				uploads := store.uploads
				store.mu.Unlock()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
				store.mu.Lock()
				afterUploads := store.uploads
				store.mu.Unlock()
				if afterUploads != uploads || readPendingRunSnapshot(t, f) != after {
					t.Fatal("stable replay repeated upload or charge")
				}
			}
		})
	}
}

func TestNativeEventPendingPublicationRecovery(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"before upload", "uncertain committed upload", "uncertain unreadable upload", "publication commit failure"} {
		t.Run(failure, func(t *testing.T) {
			store := &uncertainRunDetails{memoryRunDetails: &memoryRunDetails{}}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store})
			f.start(t)
			before := readPendingRunSnapshot(t, f)
			switch failure {
			case "before upload":
				store.rejected = true
			case "uncertain committed upload":
				store.uncertain = true
			case "uncertain unreadable upload":
				store.uncertain, store.unreadable = true, true
			case "publication commit failure":
				pendingCommitFailure(t, f)
			}
			event := pendingRunEvent(f, "pending-finish")
			response := performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event)
			if failure == "uncertain committed upload" {
				requireNativeStatus(t, response, http.StatusOK)
			} else {
				status := http.StatusServiceUnavailable
				if failure == "publication commit failure" {
					status = http.StatusInternalServerError
				}
				requireNativeStatus(t, response, status)
				if after := readPendingRunSnapshot(t, f); after != before {
					t.Fatalf("unverified content changed metadata: before=%+v after=%+v", before, after)
				}
				if got := readPublishedAttempt(t, f); got.Sequence != 1 || got.Outcome != "" {
					t.Fatal("failed publication replaced prior run")
				}
			}
			store.rejected, store.uncertain, store.unreadable, store.blocked = false, false, false, nil
			if failure == "publication commit failure" {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), `DROP TRIGGER example_publication_failure ON native_commands`); err != nil {
					t.Fatal(err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			after := readPendingRunSnapshot(t, f)
			if after.sequence != 2 || after.charged <= before.charged || after.objects != before.objects+2 {
				t.Fatalf("retry did not publish new run/history and shared receipt: before=%+v after=%+v", before, after)
			}
			writes := store.writes
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			if store.writes != writes || readPendingRunSnapshot(t, f) != after {
				t.Fatal("durable replay repeated an upload or charge")
			}
			if got := readPublishedAttempt(t, f); got.Sequence != 2 || got.Outcome != "succeeded" {
				t.Fatal("publication changed run detail")
			}
			var payloadKey string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				attempt, err := r.Attempt(t.Context(), f.attempt)
				payloadKey = attempt.PayloadObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			raw := bytes.Clone(store.data[payloadKey])
			store.mu.Unlock()
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(raw))
			if err != nil || ref.Key != payloadKey {
				t.Fatalf("run archive identity=%s want=%s error=%v", ref.Key, payloadKey, err)
			}
			var object artifactstore.ArchiveObject
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				object, err = r.ArchiveObjectByReference(t.Context(), ref)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			object.Reference.CompressedSize++
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: object.OrganizationID, ID: object.ID, Reference: object.Reference, CreatedAt: object.CreatedAt})
				return err
			}); err == nil {
				t.Fatal("archive metadata became mutable")
			}
			if got := readPublishedAttempt(t, f); got.Sequence != 2 || got.Outcome != "succeeded" {
				t.Fatal("archive mutation changed run detail")
			}
		})
	}
}
