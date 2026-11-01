package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pausedMissingRunDetails struct {
	*memoryRunDetails
	missing chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *pausedMissingRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := s.memoryRunDetails.ReadDetail(ctx, organization, ref)
	if errors.Is(err, artifact.ErrMissing) {
		s.once.Do(func() {
			close(s.missing)
			<-s.resume
		})
	}
	return raw, err
}

func TestChangeContentReadSurvivesPendingPublication(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store})
	f.start(t)
	store.fail = true
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "reader-publication"}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
	store.fail = false
	var key string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT payload_object_key FROM native_attempts WHERE id=?", f.attempt).Scan(&key); err != nil {
		t.Fatal(err)
	}
	organization := string(f.project.OrganizationID)
	ref, err := readPayloadReference(t.Context(), f.service.database.reader, organization, key)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	if err := f.service.database.reader.QueryRowContext(t.Context(), pendingContentSelection, organization, key, organization, key).Scan(&want); err != nil {
		t.Fatal(err)
	}
	paused := &pausedMissingRunDetails{memoryRunDetails: store, missing: make(chan struct{}), resume: make(chan struct{})}
	ctx := context.WithValue(t.Context(), changeContentKey{}, &changeContent{store: paused, query: f.service.database.reader})
	done := make(chan struct{})
	var resume sync.Once
	var got []byte
	var readErr error
	go func() {
		defer close(done)
		got, readErr = readChangeContentObject(ctx, organization, ref)
	}()
	t.Cleanup(func() {
		resume.Do(func() { close(paused.resume) })
		<-done
	})
	select {
	case <-paused.missing:
	case <-done:
		t.Fatalf("reader returned before the publication race: %v", readErr)
	}
	if err := f.service.convertStorageBatch(t.Context(), f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	var pending []byte
	if err := f.service.database.reader.QueryRowContext(t.Context(), pendingContentSelection, organization, key, organization, key).Scan(&pending); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("publication did not clear pending content: %v", err)
	}
	resume.Do(func() { close(paused.resume) })
	<-done
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("committed content disappeared during publication: bytes=%d want=%d error=%v", len(got), len(want), readErr)
	}
}

type uncertainRunDetails struct {
	*memoryRunDetails
	uncertain, unreadable bool
	blocked               map[string]bool
	writes                int
}

func (s *uncertainRunDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.writes++
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
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			store := &memoryRunDetails{}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store, now: func() time.Time { return now }})
			f.start(t)
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "rejected-event"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
			status, sequence := http.StatusConflict, int64(1)
			var change string
			var arguments []any
			switch name {
			case "lease":
				change, arguments = "UPDATE leases SET expires_at=? WHERE lease_id=?", []any{formatHubTime(now), f.lease.ID}
			case "fence":
				change, arguments = "UPDATE leases SET fencing_token=fencing_token+1 WHERE lease_id=?", []any{f.lease.ID}
			case "authority":
				change, arguments = "UPDATE api_tokens SET revoked_at=? WHERE id=(SELECT token_id FROM machines WHERE id=?)", []any{formatHubTime(now), f.lease.MachineID}
				status = http.StatusUnauthorized
			case "sequence":
				change, arguments = "INSERT INTO native_attempt_events(attempt_id,sequence,request_hash) VALUES(?,2,'competing-event')", []any{f.attempt}
			case "usage conflict":
				event.Data.Usage = []tracker.NativeUsage{{Provider: "openai", Model: "example-model", Input: 1, Revision: 1, From: now.Add(-time.Minute), To: now}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, event), http.StatusOK)
				event.IdempotencyKey, event.Data.Sequence, event.Data.Usage[0].Input = "conflicting-usage", 3, 2
				sequence = 2
			case "allowance":
				f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
				f.service.database.hostedOrganization = f.project.OrganizationID
				hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": 1})
				f.service.config.Hosted = nil
				event.Type, event.Data.Outcome = "run.observed", ""
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}
				status = http.StatusTooManyRequests
			case "commit failure":
				for _, statement := range []string{
					"CREATE TABLE example_commit_parent(id INTEGER PRIMARY KEY)",
					"CREATE TABLE example_commit_child(id INTEGER REFERENCES example_commit_parent(id) DEFERRABLE INITIALLY DEFERRED)",
					"CREATE TRIGGER example_commit_failure AFTER INSERT ON native_commands BEGIN INSERT INTO example_commit_child(id) VALUES(1); END",
				} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
				status = http.StatusInternalServerError
			}
			var before, beforeBytes int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_content_objects").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&beforeBytes); err != nil {
				t.Fatal(err)
			}
			uploads := store.uploads
			if change != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), change, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			request := newHubAPIRequest(t, http.MethodPost, path, f.worker, event)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, status)
			var after, afterBytes, gotSequence int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_content_objects").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&afterBytes); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_attempts WHERE id=?", f.attempt).Scan(&gotSequence); err != nil {
				t.Fatal(err)
			}
			if after != before || afterBytes != beforeBytes || store.uploads != uploads || gotSequence != sequence {
				t.Fatalf("rejection changed content: objects=%d/%d bytes=%d/%d uploads=%d/%d sequence=%d/%d", after, before, afterBytes, beforeBytes, store.uploads, uploads, gotSequence, sequence)
			}
			if name == "commit failure" && !strings.Contains(response.Body.String(), "native_operation_failed") {
				t.Fatalf("unexpected commit rejection: %s", response.Body.String())
			}
		})
	}
}

func TestNativeEventUploadsOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	for _, change := range []struct {
		name      string
		statement string
	}{
		{"fencing generation", "UPDATE leases SET fencing_token=fencing_token+1 WHERE lease_id=?"},
		{"owner", "UPDATE leases SET machine_id='replacement-machine' WHERE lease_id=?"},
		{"lease expiry", "UPDATE leases SET expires_at='2020-01-01T00:00:00Z' WHERE lease_id=?"},
		{"lease release", "UPDATE leases SET released_at='2020-01-01T00:00:00Z' WHERE lease_id=?"},
	} {
		t.Run(change.name, func(t *testing.T) {
			store := &memoryRunDetails{}
			f := claimedAttemptDiffFixture(t, false, Config{RunDetails: store})
			f.start(t)
			if change.name == "owner" {
				worker := f.nativeFixture.worker(t, "replacement-worker")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker,
					map[string]any{"id": "replacement-machine", "hostname": "replacement", "display_name": "Replacement", "version": "test", "capacity": 1}), http.StatusOK)
			}
			var writing bool
			timing := &requestTiming{writerTransaction: func(active bool) { writing = active }}
			uploaded, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var pause, release sync.Once
			store.afterStore = func(attachment.ArchiveReference) {
				if writing {
					t.Error("uploaded while holding the writer")
				}
				pause.Do(func() {
					close(uploaded)
					<-resume
				})
			}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "committed-finish"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
			request := newHubAPIRequest(t, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
			response := httptest.NewRecorder()
			go func() {
				defer close(done)
				f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			}()
			t.Cleanup(func() {
				release.Do(func() { close(resume) })
				<-done
			})
			select {
			case <-uploaded:
			case <-done:
				t.Fatalf("request returned before upload: status=%d body=%s", response.Code, response.Body.String())
			}
			var sequence int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_attempts WHERE id=?", f.attempt).Scan(&sequence); err != nil || sequence != 2 {
				t.Fatalf("uploaded before commit: sequence=%d error=%v", sequence, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), change.statement, f.lease.ID); err != nil {
				t.Fatal(err)
			}
			release.Do(func() { close(resume) })
			<-done
			requireNativeStatus(t, response, http.StatusOK)
			var pending int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE pending_content IS NOT NULL").Scan(&pending); err != nil || pending != 2 {
				t.Fatalf("superseded publisher cleared pending content: pending=%d want=2 error=%v", pending, err)
			}
			if got := readReferencedRun(t, f.service, f.attempt); got.Sequence != 2 || got.Outcome != "succeeded" {
				t.Fatal("supersession lost committed run history")
			}
			uploads := store.uploads
			if err := f.service.convertStorageBatch(t.Context(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE pending_content IS NOT NULL").Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("conversion left pending=%d: %v", pending, err)
			}
			if store.uploads != uploads {
				t.Fatal("conversion repeated a stable-key upload")
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
			var beforeBytes int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&beforeBytes); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "before upload":
				store.fail = true
			case "uncertain committed upload":
				store.uncertain = true
			case "uncertain unreadable upload":
				store.uncertain, store.unreadable = true, true
			case "publication commit failure":
				for _, statement := range []string{
					"CREATE TABLE example_publication_parent(id INTEGER PRIMARY KEY)",
					"CREATE TABLE example_publication_child(id INTEGER REFERENCES example_publication_parent(id) DEFERRABLE INITIALLY DEFERRED)",
					"CREATE TRIGGER example_publication_failure AFTER UPDATE OF pending_content ON attachments BEGIN INSERT INTO example_publication_child(id) VALUES(1); END",
				} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "pending-finish"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, event), http.StatusOK)
			var pending, references int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE pending_content IS NOT NULL").Scan(&pending); err != nil {
				t.Fatal(err)
			}
			wantPending := 2
			if failure == "uncertain committed upload" {
				wantPending = 0
			}
			if pending != wantPending {
				t.Fatalf("pending=%d, want %d", pending, wantPending)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_archive_references WHERE work_item_id=?", f.issue.WorkItemID).Scan(&references); err != nil || references != 4 {
				t.Fatalf("references=%d, want 4: %v", references, err)
			}
			stored := readReferencedRun(t, f.service, f.attempt)
			if stored.Sequence != 2 || stored.Outcome != "succeeded" {
				t.Fatalf("pending run unreadable: %+v", stored)
			}
			writes := store.writes
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, event), http.StatusOK)
			if store.writes != writes {
				t.Fatal("durable replay repeated an upload")
			}
			store.fail, store.uncertain, store.unreadable, store.blocked = false, false, false, nil
			if failure == "publication commit failure" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER example_publication_failure"); err != nil {
					t.Fatal(err)
				}
			}
			var chargedBytes int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&chargedBytes); err != nil || chargedBytes <= beforeBytes {
				t.Fatalf("committed pending content was not charged: before=%d after=%d error=%v", beforeBytes, chargedBytes, err)
			}
			if err := f.service.convertStorageBatch(t.Context(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM attachments WHERE pending_content IS NOT NULL)+(SELECT count(*) FROM change_content_objects WHERE pending_content IS NOT NULL)").Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("conversion left pending=%d: %v", pending, err)
			}
			if failure != "before upload" && store.writes != writes {
				t.Fatal("conversion re-uploaded content already present by stable key")
			}
			var afterBytes int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&afterBytes); err != nil || afterBytes != chargedBytes {
				t.Fatalf("publication charged twice: before=%d after=%d error=%v", chargedBytes, afterBytes, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE attachments SET object_key=object_key WHERE kind='archive'"); err == nil {
				t.Fatal("archive metadata became mutable")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE attachments SET pending_content=? WHERE kind='archive'", []byte("replacement")); err == nil {
				t.Fatal("published archive became pending again")
			}
			if got := readReferencedRun(t, f.service, f.attempt); got.Sequence != 2 || got.Outcome != "succeeded" {
				t.Fatal("publication changed run detail")
			}
		})
	}
}
