package postgres_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type historyObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newHistoryObjects() *historyObjects { return &historyObjects{objects: make(map[string][]byte)} }
func (s *historyObjects) StoreDetail(_ context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(org, 1, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ref.Key] = bytes.Clone(compressed)
	return ref, nil
}
func (s *historyObjects) ReadDetail(_ context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(org) != nil {
		return nil, artifact.ErrIntegrity
	}
	s.mu.Lock()
	compressed, ok := s.objects[ref.Key]
	compressed = bytes.Clone(compressed)
	s.mu.Unlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(r)
	return raw, errors.Join(err, r.Close())
}

func historyJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func historyActor() tracker.Actor {
	return tracker.Actor{Kind: "human", PrincipalID: "example", DisplayName: "Example actor", ViaKey: "example-key"}
}
func historyAt() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.UTC) }
func historyEvent(t *testing.T, c *eventstore.Content, org tracker.OrganizationID, id string) eventstore.Event {
	t.Helper()
	value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "issue.edited", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}, Reason: "Example reason"}}
	row, err := c.PrepareEvent(t.Context(), org, historyJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func historyEventForItem(t *testing.T, c *eventstore.Content, org tracker.OrganizationID, id string, item tracker.NativeWorkItemID) eventstore.Event {
	t.Helper()
	value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: item, Type: "issue.edited", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}, Reason: "Example reason"}}
	row, err := c.PrepareEvent(t.Context(), org, historyJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

type historyFixture struct {
	content  *eventstore.Content
	objects  *historyObjects
	event    eventstore.Event
	snapshot eventstore.CollaborationVersion
	change   eventstore.Change
	version  eventstore.Version
	evidence eventstore.Evidence
	link     eventstore.Link
	receipt  eventstore.Receipt
}

func seedHistory(t *testing.T, r *postgres.Repository, org tracker.OrganizationID, suffix string) historyFixture {
	t.Helper()
	f := historyFixture{objects: newHistoryObjects()}
	f.content = eventstore.NewContent(f.objects)
	f.event = historyEvent(t, f.content, org, "event_"+suffix)
	f.event.Sequence = 0
	change := tracker.ChangeRequest{ID: "change_" + suffix, OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", LinkedIssues: []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"}, Title: "Example change", Body: "Change body", Revision: 1, CreatedAt: historyAt(), UpdatedAt: historyAt()}
	var err error
	f.change, err = f.content.PrepareChange(t.Context(), org, historyJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	version := tracker.ChangeVersion{ID: "version_" + suffix, ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", MergeBaseSHA: "merge-base", Repository: "owner/repository", PolicyID: "policy", AttemptID: "attempt", External: &tracker.ChangeExternalReference{Provider: "example", ID: "external", URL: "https://example.com/change"}}, Actor: historyActor(), CreatedAt: historyAt()}
	version.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: "base", HeadSHA: "head", BundleSHA256: artifact.Digest([]byte(string(org) + suffix)), DiffSHA256: artifact.Digest([]byte("diff")), Bytes: 123}
	f.version, err = f.content.PrepareVersion(t.Context(), org, historyJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Evidence body", Actor: historyActor(), CreatedAt: historyAt(), Provenance: &tracker.Provenance{Provider: "example", ExternalID: "source_" + suffix, AuthorID: "author"}}
	f.evidence, err = f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "discussion"}, historyJSON(t, discussion))
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot, err = f.content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: change.WorkItemID, RecordID: "snapshot_" + suffix, Revision: 2}, historyJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	f.receipt, err = f.content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "example", Operation: "create_change", CommandKey: suffix, RequestHash: artifact.Digest([]byte("request")), CreatedAt: historyAt()}, []byte(`{"change_id":"`+change.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	f.link = eventstore.Link{OrganizationID: org, ProjectID: change.ProjectID, WorkItemID: "wi_first_2", ChangeID: change.ID}
	f.link.CreatedAt = f.change.CreatedAt
	if err := r.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
		if f.event, err = w.AppendEvent(t.Context(), f.event); err != nil {
			return err
		}
		if f.snapshot, err = w.CreateCollaborationVersion(t.Context(), f.snapshot); err != nil {
			return err
		}
		if f.change, err = w.CreateChange(t.Context(), f.change); err != nil {
			return err
		}
		if f.version, err = w.CreateVersion(t.Context(), f.version); err != nil {
			return err
		}
		if f.evidence, err = w.AppendEvidence(t.Context(), f.evidence); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), f.link); err != nil {
			return err
		}
		f.receipt, err = w.PutReceipt(t.Context(), f.receipt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.change.EvidenceSequence = 1
	return f
}

func TestEventChangeTenantRoundTrip(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		for _, suffix := range []string{"first", "second"} {
			t.Run(string(org)+"/"+suffix, func(t *testing.T) {
				f := seedHistory(t, r, org, suffix)
				if f.event.Sequence < 1 || f.evidence.Sequence != 1 {
					t.Fatalf("missing sequences: event=%d evidence=%d", f.event.Sequence, f.evidence.Sequence)
				}
				var events []eventstore.Event
				var snapshots eventstore.CollaborationVersion
				var change eventstore.Change
				var changes []eventstore.Change
				var versions []eventstore.Version
				var evidence []eventstore.Evidence
				var links []eventstore.Link
				var receipt eventstore.Receipt
				err := r.ReadRecords(t.Context(), org, corestore.Serving, func(reader eventstore.Reader) error {
					var err error
					if events, err = reader.Events(t.Context(), "prj_first", "wi_first_1", f.event.Sequence-1, 1); err != nil {
						return err
					}
					if snapshots, err = reader.CollaborationVersion(t.Context(), f.snapshot.RecordID, 2); err != nil {
						return err
					}
					if change, err = reader.Change(t.Context(), f.change.ID); err != nil {
						return err
					}
					after := ""
					if suffix == "second" {
						after = "change_first"
					}
					if changes, err = reader.Changes(t.Context(), "prj_first", "wi_first_1", after, 1); err != nil {
						return err
					}
					if versions, err = reader.Versions(t.Context(), f.change.ID, 0, 1); err != nil {
						return err
					}
					if evidence, err = reader.Evidence(t.Context(), f.change.ID, 0, 1); err != nil {
						return err
					}
					if receipt, err = reader.Receipt(t.Context(), f.receipt.ActorID, f.receipt.Operation, f.receipt.CommandKey); err != nil {
						return err
					}
					links, err = reader.Links(t.Context(), f.change.ID, "", 1)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct {
					name      string
					got, want any
				}{
					{"events", events, []eventstore.Event{f.event}},
					{"snapshot", snapshots, f.snapshot},
					{"change", change, f.change},
					{"changes", changes, []eventstore.Change{f.change}},
					{"versions", versions, []eventstore.Version{f.version}},
					{"evidence", evidence, []eventstore.Evidence{f.evidence}},
					{"links", links, []eventstore.Link{f.link}},
					{"receipt", receipt, f.receipt},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if !reflect.DeepEqual(tc.got, tc.want) {
							t.Fatalf("round trip mismatch: got %+v want %+v", tc.got, tc.want)
						}
					})
				}
				response, err := f.content.Receipt(t.Context(), receipt)
				if err != nil || string(response) != `{"change_id":"`+f.change.ID+`"}` {
					t.Fatalf("receipt response=%s error=%v", response, err)
				}
				event, err := f.content.Event(t.Context(), events[0])
				if err != nil || event.Data.Reason != "Example reason" || event.Actor.DisplayName != "Example actor" || event.AggregateSequence != f.event.Sequence {
					t.Fatalf("event payload=%+v error=%v", event, err)
				}
				hydrated, err := f.content.Change(t.Context(), change)
				if err != nil || hydrated.Body != "Change body" || len(hydrated.LinkedIssues) != 2 {
					t.Fatalf("change payload=%+v error=%v", hydrated, err)
				}
				version, err := f.content.Version(t.Context(), versions[0])
				if err != nil || version.External == nil || version.External.ID != "external" || version.AttemptID != "attempt" {
					t.Fatalf("version payload=%+v error=%v", version, err)
				}
				wantSource := tracker.ChangeSource{Format: "git-bundle", BaseSHA: "base", HeadSHA: "head", BundleSHA256: artifact.Digest([]byte(string(org) + suffix)), DiffSHA256: artifact.Digest([]byte("diff")), Bytes: 123}
				if version.Source == nil || *version.Source != wantSource {
					t.Fatalf("portable source manifest=%+v want=%+v", version.Source, wantSource)
				}
				raw, err := f.content.Evidence(t.Context(), evidence[0])
				var discussion tracker.ChangeDiscussion
				if err != nil || json.Unmarshal(raw, &discussion) != nil || discussion.Body != "Evidence body" || discussion.Provenance.AuthorID != "author" {
					t.Fatalf("evidence payload=%s error=%v", raw, err)
				}
				raw, err = f.content.CollaborationVersion(t.Context(), snapshots)
				var snapshot tracker.ChangeRequest
				if err != nil || json.Unmarshal(raw, &snapshot) != nil || snapshot.Body != "Change body" {
					t.Fatalf("snapshot payload=%s error=%v", raw, err)
				}
			})
		}
	}
}

func TestLatestRunEventUsesTypedAttemptIndex(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		f := seedHistory(t, r, org, "latest-run-event")
		content := eventstore.NewContent(f.objects)
		appendRunEvent := func(id, attempt string) eventstore.Event {
			t.Helper()
			run := tracker.NativeRunData{AttemptID: attempt, RunID: "run_" + attempt, LeaseID: tracker.LeaseID("lease_" + attempt), FencingToken: 1, Sequence: 1}
			if attempt == "attempt_target" {
				run.Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Waiting: true, ChangeID: "change_target", VersionID: "version_target", HeadSHA: "head_target"}}
			}
			value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "run.observed", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt(), Data: tracker.CollaborationData{Run: &run}}
			record, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			var stored eventstore.Event
			if err := r.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
				stored, err = w.AppendEvent(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return stored
		}
		wanted := appendRunEvent("run_target", "attempt_target")
		appendRunEvent("run_later", "attempt_other")
		var got eventstore.Event
		if err := r.ReadRecords(t.Context(), org, corestore.Serving, func(reader eventstore.Reader) error {
			var err error
			got, err = reader.LatestRunEvent(t.Context(), "prj_first", "attempt_target")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.ID != wanted.ID || got.Sequence != wanted.Sequence || got.AttemptID != "attempt_target" || !got.LandingWaiting || got.LandingChangeID != "change_target" || got.LandingVersionID != "version_target" || got.LandingHeadSHA != "head_target" {
			t.Fatalf("latest run event = %+v, want %+v", got, wanted)
		}
		hydrated, err := content.Event(t.Context(), got)
		if err != nil || hydrated.Data.Run == nil || hydrated.Data.Run.Runtime == nil || hydrated.Data.Run.Runtime.Landing == nil || !hydrated.Data.Run.Runtime.Landing.Waiting {
			t.Fatalf("hydrated latest run event = %+v, error %v", hydrated, err)
		}
	}
}

func TestLatestRunEventsBatchBoundsHistoryVisits(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	attempts := make([]string, 17)
	for i := range attempts {
		attempts[i] = fmt.Sprintf("attempt_%d", i+1)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events
			(organization_id,project_id,work_item_id,id,sequence,schema_version,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			SELECT $1,'prj_first','wi_first_1','event_'||n,n,1,
			CASE WHEN n<=1700 THEN 'attempt_'||((n-1)%17+1) ELSE 'unrelated_'||n END,
			$2::timestamptz-n*interval '1 second',
			'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
			FROM generate_series(1,30000) n`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
		t.Fatal(err)
	}
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
				t.Fatal(err)
			}
			probe := &searchPlanProbe{DBTX: tx}
			queries := pgquery.New(probe)
			params := pgquery.RecordLatestRunEventsByAttemptsParams{OrganizationID: org, ProjectID: "prj_first", AttemptIds: attempts}
			rows, err := queries.RecordLatestRunEventsByAttempts(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(attempts) {
				t.Fatalf("latest events=%d, want %d", len(rows), len(attempts))
			}
			seen := make(map[string]bool)
			for _, row := range rows {
				var number int
				if _, err := fmt.Sscanf(row.AttemptID, "attempt_%d", &number); err != nil {
					t.Fatal(err)
				}
				want := int64(1683 + number)
				if number < 1 || number > len(attempts) || seen[row.AttemptID] || row.OrganizationID != org || row.ProjectID != "prj_first" || row.Sequence != want || row.ID != fmt.Sprintf("event_%d", want) {
					t.Fatalf("latest event lost sequence or scope: %+v", row)
				}
				seen[row.AttemptID] = true
			}
			var raw []byte
			if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.statement, probe.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct{ Plan searchPlanNode }
			if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
				t.Fatalf("decode plan: %v", err)
			}
			var visits func(searchPlanNode) float64
			visits = func(node searchPlanNode) float64 {
				var count float64
				if node.Relation == "collaboration_events" {
					count = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
				}
				for _, child := range node.Plans {
					count += visits(child)
				}
				return count
			}
			visited := visits(plan[0].Plan)
			t.Logf("requested=%d actual event rows visited=%.0f plan=%s", len(attempts), visited, raw)
			if visited > float64(4*len(attempts)) {
				t.Errorf("latest batch visited %.0f event rows for %d attempts", visited, len(attempts))
			}
			params.OrganizationID = "org_foreign"
			foreign, err := queries.RecordLatestRunEventsByAttempts(t.Context(), params)
			if err != nil || len(foreign) != 0 {
				t.Fatalf("foreign tenant events=%d error=%v", len(foreign), err)
			}
			params.OrganizationID, params.ProjectID = org, "prj_second"
			otherProject, err := queries.RecordLatestRunEventsByAttempts(t.Context(), params)
			if err != nil || len(otherProject) != 0 {
				t.Fatalf("other project events=%d error=%v", len(otherProject), err)
			}
		})
	}
}

func TestEventChangeReceipts(t *testing.T) {
	admin := template.Database(t)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, "org_first", historyAt()); err != nil {
		t.Fatal(err)
	}
	r, _ := repositoryFor(t, admin, smallPool())
	seedRepository(t, r, "org_first")
	f := seedHistory(t, r, "org_first", "first")
	for _, tc := range []struct {
		name   string
		replay func(eventstore.Writer, bool) error
	}{
		{"event", func(w eventstore.Writer, changed bool) error {
			record := f.event
			record.Sequence = 0
			if changed {
				record.HeadSHA = "other"
			}
			got, err := w.AppendEvent(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.event) {
				t.Error("event receipt changed")
			}
			return err
		}},
		{"snapshot", func(w eventstore.Writer, changed bool) error {
			record := f.snapshot
			if changed {
				record.WorkItemID = "wi_first_2"
			}
			_, err := w.CreateCollaborationVersion(t.Context(), record)
			return err
		}},
		{"change", func(w eventstore.Writer, changed bool) error {
			record := f.change
			if changed {
				record.Title = "other"
			}
			_, err := w.CreateChange(t.Context(), record)
			return err
		}},
		{"version", func(w eventstore.Writer, changed bool) error {
			record := f.version
			if changed {
				record.HeadSHA = "other"
			}
			_, err := w.CreateVersion(t.Context(), record)
			return err
		}},
		{"evidence", func(w eventstore.Writer, changed bool) error {
			record := f.evidence
			record.Sequence = 0
			if changed {
				record.Decision = "other"
			}
			got, err := w.AppendEvidence(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.evidence) {
				t.Error("evidence receipt changed")
			}
			return err
		}},
		{"link", func(w eventstore.Writer, changed bool) error {
			record := f.link
			if changed {
				record.ProjectID = "prj_second"
			}
			return w.LinkIssue(t.Context(), record)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, changed := range []bool{false, true} {
				err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error { return tc.replay(w, changed) })
				if changed && !errors.Is(err, corestore.ErrRevisionConflict) || !changed && err != nil {
					t.Fatalf("changed=%v error=%v", changed, err)
				}
			}
		})
	}
	next := historyEvent(t, f.content, "org_first", "event_next")
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		got, err := w.AppendEvent(t.Context(), next)
		if err == nil && got.Sequence != 2 {
			t.Errorf("replays consumed sequence: %d", got.Sequence)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLandingReceiptIsImmutableAndTenantScoped(t *testing.T) {
	admin := template.Database(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, organization, historyAt()); err != nil {
			t.Fatal(err)
		}
	}
	r, pool := repositoryFor(t, admin, smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, organization)
	}
	if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_first", ID: "machine_1", Hostname: "example", Capacity: 1, Version: "example"})
	}); err != nil {
		t.Fatal(err)
	}
	f := seedHistory(t, r, "org_first", "first")
	lease := claimLease(t, r, "org_first", leaseClaim(1))
	attempt := attemptEvent(lease)
	attempt.ID = f.version.AttemptID
	storedAttempt := recordLeaseAttempt(t, r, "org_first", attempt)
	value := tracker.NativeLandingReceipt{VersionID: f.version.ID, MergeSHA: strings.Repeat("a", 40), BaseRef: "main", Landed: true}
	record, err := f.content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: "org_first", AttemptID: storedAttempt.ID, VersionID: f.version.ID, Repository: f.version.Repository, BaseRef: value.BaseRef}, value)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		got, err := w.CreateLandingReceipt(t.Context(), record)
		if err == nil && got != record {
			t.Errorf("created landing receipt = %+v, want %+v", got, record)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFor(t, pool, pool)
	for _, statement := range []string{"UPDATE change_landing_receipts SET merge_sha=merge_sha", "DELETE FROM change_landing_receipts"} {
		tx := tenantTx(t, runtime, pool, "org_first")
		if _, err := tx.ExecContext(t.Context(), statement); err == nil {
			t.Fatalf("immutable landing receipt accepted %q", statement)
		} else {
			requireSQLState(t, err, "23514")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		got, err := w.CreateLandingReceipt(t.Context(), record)
		if err == nil && got != record {
			t.Errorf("replayed landing receipt = %+v, want %+v", got, record)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.MergeSHA = strings.Repeat("b", 40)
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		_, err := w.CreateLandingReceipt(t.Context(), changed)
		return err
	}); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("changed receipt error = %v, want revision conflict", err)
	}
	wrongAttempt := record
	wrongAttempt.AttemptID = "attempt_unrelated"
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		_, err := w.CreateLandingReceipt(t.Context(), wrongAttempt)
		return err
	}); !errors.Is(err, eventstore.ErrInvalidRecord) {
		t.Fatalf("unrelated attempt receipt error = %v, want invalid record", err)
	}
	if err := r.ReadRecords(t.Context(), "org_second", corestore.Serving, func(reader eventstore.Reader) error {
		_, err := reader.LandingReceipt(t.Context(), storedAttempt.ID)
		return err
	}); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("cross-tenant receipt read error = %v, want not found", err)
	}
	var got eventstore.LandingReceipt
	if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
		var err error
		got, err = reader.LandingReceipt(t.Context(), storedAttempt.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var latest eventstore.LandingReceipt
	if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
		var err error
		latest, err = reader.LatestLandingReceipt(t.Context(), f.change.ProjectID, f.change.WorkItemID)
		return err
	}); err != nil || latest != record {
		t.Fatalf("latest landing receipt = %+v, %v; want %+v", latest, err, record)
	}
	if err := r.ReadRecords(t.Context(), "org_second", corestore.Serving, func(reader eventstore.Reader) error {
		_, err := reader.LatestLandingReceipt(t.Context(), f.change.ProjectID, f.change.WorkItemID)
		return err
	}); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("cross-tenant latest landing receipt error = %v, want not found", err)
	}
	hydrated, err := f.content.LandingReceipt(t.Context(), got)
	if err != nil || !reflect.DeepEqual(hydrated, value) {
		t.Fatalf("landing payload = %+v, %v; want %+v", hydrated, err, value)
	}
}

func TestLatestChangeUsesLinkCreationChronology(t *testing.T) {
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, organization)
		content := eventstore.NewContent(newHistoryObjects())
		olderAt := historyAt()
		entries := make([]struct {
			id string
			at time.Time
		}, 0, 202)
		entries = append(entries, struct {
			id string
			at time.Time
		}{id: "z_oldest", at: olderAt})
		for index := range 200 {
			entries = append(entries, struct {
				id string
				at time.Time
			}{id: fmt.Sprintf("m_%03d", index), at: olderAt.Add(time.Duration(index+1) * time.Second)})
		}
		newerAt := olderAt.Add(201 * time.Second)
		entries = append(entries, struct {
			id string
			at time.Time
		}{id: "a_newest", at: newerAt})
		rows := make([]eventstore.Change, 0, len(entries))
		for _, entry := range entries {
			value := tracker.ChangeRequest{ID: entry.id, OrganizationID: organization, ProjectID: "prj_first", WorkItemID: "wi_first_1", LinkedIssues: []tracker.NativeWorkItemID{"wi_first_1"}, Title: entry.id, Body: "Example", Revision: 1, CreatedAt: entry.at, UpdatedAt: entry.at}
			record, err := content.PrepareChange(t.Context(), organization, historyJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, record)
		}
		if err := r.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
			for _, record := range rows {
				_, err := w.CreateChange(t.Context(), record)
				if err != nil {
					return err
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: organization, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ChangeID: record.ID}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var latest eventstore.Change
		if err := r.ReadRecords(t.Context(), organization, corestore.Serving, func(reader eventstore.Reader) error {
			var err error
			latest, err = reader.LatestChange(t.Context(), "prj_first", "wi_first_1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if latest.ID != "a_newest" || !latest.CreatedAt.Equal(newerAt) {
			t.Fatalf("latest change = %+v, want newest timestamp despite lexically smaller ID and over 200 links", latest)
		}
	}
}

func TestEventChangeImmutableHistory(t *testing.T) {
	admin := template.Database(t)
	r, pool := repositoryFor(t, admin, smallPool())
	seedRepository(t, r, "org_first")
	seedHistory(t, r, "org_first", "first")
	runtime := runtimeFor(t, pool, pool)
	for _, table := range []string{"collaboration_events", "collaboration_versions", "change_versions", "change_evidence", "native_commands"} {
		for _, op := range []string{"update", "delete"} {
			t.Run(table+"/"+op, func(t *testing.T) {
				tx := tenantTx(t, runtime, pool, "org_first")
				statement := "DELETE FROM " + table
				if op == "update" {
					statement = "UPDATE " + table + " SET payload_object_key=payload_object_key"
				}
				_, err := tx.ExecContext(t.Context(), statement)
				requireSQLState(t, err, "23514")
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestEventChangeSequenceContention(t *testing.T) {
	for _, kind := range []string{"event", "event replay", "evidence", "evidence replay"} {
		t.Run(kind, func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			seedRepository(t, r, "org_first")
			f := seedHistory(t, r, "org_first", "first")
			preparedEvents := []eventstore.Event{historyEvent(t, f.content, "org_first", "contended1"), historyEvent(t, f.content, "org_first", "contended2")}
			if kind == "event replay" {
				preparedEvents[1] = preparedEvents[0]
			}
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			ready := make(chan struct{})
			var firstSequence, secondSequence int64
			appendRecord := func(ctx context.Context, w eventstore.Writer, index int) (int64, error) {
				switch kind {
				case "event", "event replay":
					event := preparedEvents[index-1]
					got, err := w.AppendEvent(ctx, event)
					return got.Sequence, err
				default:
					evidence := f.evidence
					evidence.Sequence = 0
					evidence.SourceKey = new("contended")
					if kind == "evidence" {
						evidence.SourceKey = new(fmt.Sprint(index))
					}
					got, err := w.AppendEvidence(ctx, evidence)
					return got.Sequence, err
				}
			}
			first := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					firstSequence, err = appendRecord(ctx, w, 1)
					if err != nil {
						return err
					}
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			waitRepositorySignal(t, ready)
			second := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					secondSequence, err = appendRecord(ctx, w, 2)
					return err
				})
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("second append has not reached the parent lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := first.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := second.wait(t); err != nil {
				t.Fatal(err)
			}
			want := firstSequence + 1
			if kind == "event replay" || kind == "evidence replay" {
				want = firstSequence
			}
			if firstSequence != 2 || secondSequence != want {
				t.Fatalf("sequences=(%d,%d), want (2,%d)", firstSequence, secondSequence, want)
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				switch kind {
				case "event", "event replay":
					records, err := reader.Events(t.Context(), "prj_first", "wi_first_1", 1, 10)
					if err == nil && (len(records) != int(want-1) || records[0].Sequence != 2 || records[len(records)-1].Sequence != want) {
						t.Errorf("event order=%+v", records)
					}
					return err
				default:
					records, err := reader.Evidence(t.Context(), f.change.ID, 1, 10)
					if err == nil && (len(records) != int(want-1) || records[0].Sequence != 2 || records[len(records)-1].Sequence != want) {
						t.Errorf("evidence order=%+v", records)
					}
					return err
				}
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProjectEventCursorSerializesDifferentIssuesWithoutReplayGaps(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
	seedAccessTarget(t, admin, r, "org_first")
	f := seedHistory(t, r, "org_first", "project_cursor")
	firstEvent := historyEventForItem(t, f.content, "org_first", "project_cursor_first", "wi_first_1")
	secondEvent := historyEventForItem(t, f.content, "org_first", "project_cursor_second", "wi_first_2")
	firstSequence, secondSequence := int64(0), int64(0)
	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	first := startRepositoryTask(t, func(ctx context.Context) error {
		return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
			stored, err := w.AppendEvent(ctx, firstEvent)
			if err != nil {
				return err
			}
			firstSequence = stored.ProjectSequence
			close(ready)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	waitRepositorySignal(t, ready)
	second := startRepositoryTask(t, func(ctx context.Context) error {
		return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
			stored, err := w.AppendEvent(ctx, secondEvent)
			if err != nil {
				return err
			}
			secondSequence = stored.ProjectSequence
			return nil
		})
	})
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
			return err
		}
		if !waiting {
			return errors.New("second project event has not reached the cursor lock")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := first.wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(t); err != nil {
		t.Fatal(err)
	}
	if firstSequence != 2 || secondSequence != 3 {
		t.Fatalf("project event sequences=(%d,%d), want=(2,3)", firstSequence, secondSequence)
	}
	var changes []eventstore.ProjectChange
	var highwater int64
	if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
		var err error
		highwater, err = reader.ProjectEventSequence(t.Context(), "prj_first")
		if err != nil {
			return err
		}
		changes, err = reader.ProjectChanges(t.Context(), "prj_first", 1, highwater, historyAt().Add(time.Hour), 201)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if highwater != 3 || len(changes) != 2 || changes[0].WorkItemID != "wi_first_1" || changes[1].WorkItemID != "wi_first_2" {
		t.Fatalf("project change page highwater=%d changes=%+v", highwater, changes)
	}
	var replay eventstore.Event
	if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
		var err error
		replay, err = w.AppendEvent(t.Context(), firstEvent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if replay.ProjectSequence != firstSequence {
		t.Fatalf("event replay project sequence=%d, want=%d", replay.ProjectSequence, firstSequence)
	}
	if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
		sequence, err := reader.ProjectEventSequence(t.Context(), "prj_first")
		if err == nil && sequence != highwater {
			t.Errorf("replay advanced project cursor to %d, want %d", sequence, highwater)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventChangeImportedSequenceContinuation(t *testing.T) {
	for _, kind := range []string{"event", "evidence"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			f := seedHistory(t, r, "org_first", "first")
			for index, sequence := range []int64{40, 12, 0} {
				record := historyEvent(t, f.content, "org_first", fmt.Sprintf("import_%d", index))
				record.Sequence = sequence
				err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var got int64
					if kind == "event" {
						stored, err := w.AppendEvent(t.Context(), record)
						if err != nil {
							return err
						}
						got = stored.Sequence
						replay, err := w.AppendEvent(t.Context(), record)
						if err != nil {
							return err
						}
						if replay.Sequence != got {
							t.Error("import replay changed event sequence")
						}
					} else {
						record := f.evidence
						record.Sequence = sequence
						record.SourceKey = nil
						stored, err := w.AppendEvidence(t.Context(), record)
						if err != nil {
							return err
						}
						got = stored.Sequence
						if sequence != 0 {
							replay, err := w.AppendEvidence(t.Context(), record)
							if err != nil {
								return err
							}
							if replay.Sequence != got {
								t.Error("import replay changed evidence sequence")
							}
						}
					}
					want := sequence
					if sequence == 0 {
						want = 41
					}
					if got != want {
						t.Errorf("allocated=%d want=%d", got, want)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				var sequences []int64
				if kind == "event" {
					rows, err := reader.Events(t.Context(), "prj_first", "wi_first_1", 1, 10)
					if err != nil {
						return err
					}
					for _, row := range rows {
						sequences = append(sequences, row.Sequence)
					}
				} else {
					rows, err := reader.Evidence(t.Context(), f.change.ID, 1, 10)
					if err != nil {
						return err
					}
					for _, row := range rows {
						sequences = append(sequences, row.Sequence)
					}
				}
				if !reflect.DeepEqual(sequences, []int64{12, 40, 41}) {
					t.Errorf("history order=%v", sequences)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventChangeCommandReceiptContention(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_request_%v", changed), func(t *testing.T) {
			admin := template.Database(t)
			r, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			content := eventstore.NewContent(newHistoryObjects())
			first, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "example", Operation: "create_change", CommandKey: "concurrent", RequestHash: artifact.Digest([]byte("first"))}, []byte(`{"result":"original"}`))
			if err != nil {
				t.Fatal(err)
			}
			next, err := content.PrepareReceipt(t.Context(), first, []byte(`{"result":"later"}`))
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				next.RequestHash = artifact.Digest([]byte("different"))
			}
			ready, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var receipt eventstore.Receipt
			holder := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error {
					var err error
					first, err = w.PutReceipt(ctx, first)
					if err != nil {
						return err
					}
					close(ready)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			waitRepositorySignal(t, ready)
			waiter := startRepositoryTask(t, func(ctx context.Context) error {
				return r.WriteRecords(ctx, "org_first", corestore.Serving, func(w eventstore.Writer) error { var err error; receipt, err = w.PutReceipt(ctx, next); return err })
			})
			testenv.WaitUntil(t, t.Context(), func() error {
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("receipt writer has not reached the unique key lock")
				}
				return nil
			})
			releaseOnce.Do(func() { close(release) })
			if err := holder.wait(t); err != nil {
				t.Fatal(err)
			}
			err = waiter.wait(t)
			if changed {
				if !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("changed hash error=%v", err)
				}
			} else if err != nil || !reflect.DeepEqual(receipt, first) {
				t.Fatalf("replayed receipt=%+v error=%v", receipt, err)
			}
			if err := r.ReadRecords(t.Context(), "org_first", corestore.Serving, func(reader eventstore.Reader) error {
				var err error
				receipt, err = reader.Receipt(t.Context(), first.ActorID, first.Operation, first.CommandKey)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := content.Receipt(t.Context(), receipt)
			if err != nil || string(raw) != `{"result":"original"}` {
				t.Fatalf("durable receipt=%s error=%v", raw, err)
			}
		})
	}
}

func TestChangeSummaryVisitsCurrentActorsInsteadOfReviewHistory(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, actors := range []int{17, 10000} {
			key := fmt.Sprintf("current_%d", actors)
			fixture := seedHistory(t, repository, org, key)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence
    (organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_session_known)
    SELECT $1,$2,n+1,$3,'review','human','actor_'||lpad(((n-1)%$7)::text,5,'0'),
    CASE WHEN n%37=0 THEN 'commented' WHEN ((n-1)%$7)%2=0 THEN 'changes_requested' ELSE 'approved' END,
    $4::timestamptz,$5,$6,1,1,'human',true FROM generate_series(1,30000) n`, org, fixture.change.ID, fixture.version.ID, historyAt(), fixture.evidence.PayloadObjectKey, fixture.evidence.PayloadSHA256, actors); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO current_change_reviews(organization_id,change_id,version_id,review_kind,actor_key,decision,evidence_sequence)
    SELECT DISTINCT ON (actor_principal_id) organization_id,change_id,version_id,'human',actor_principal_id,decision,sequence FROM change_evidence
    WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND decision<>'commented'
    ORDER BY actor_principal_id,sequence DESC`, org, fixture.change.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET evidence_sequence=30001,review_summary_sequence=30001,current_version_id=$3,review_evidence_revision=review_evidence_revision+1 WHERE organization_id=$1 AND id=$2`, org, fixture.change.ID, fixture.version.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if _, err := admin.ExecContext(t.Context(), `ANALYZE change_evidence; ANALYZE current_change_reviews`); err != nil {
				t.Fatal(err)
			}
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, actors := range []int{17, 10000} {
				excluded := "actor_00011"
				if actors == 10000 {
					excluded = "actor_09999"
				}
				for _, actor := range []string{"", excluded} {
					t.Run(fmt.Sprintf("%s/analyzed=%v/actors=%d/exclude=%s", org, analyzed, actors, actor), func(t *testing.T) {
						tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
							t.Fatal(err)
						}
						probe := &searchPlanProbe{DBTX: tx}
						queries := pgquery.New(probe)
						args := pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: fmt.Sprintf("change_current_%d", actors), VersionID: fmt.Sprintf("version_current_%d", actors), ReviewingActor: actor, Through: 30001}
						sequences, err := queries.RecordCurrentChangeReviewSequences(t.Context(), args)
						if err != nil {
							t.Fatal(err)
						}
						headSQL, headArgs := probe.statement, append([]any(nil), probe.args...)
						rows, err := queries.RecordCurrentChangeEvidenceBySequences(t.Context(), pgquery.RecordCurrentChangeEvidenceBySequencesParams{OrganizationID: org, ChangeID: args.ChangeID, Sequences: sequences})
						if err != nil {
							t.Fatal(err)
						}
						expected := []int64{30000, 30001}
						if actor != "" {
							expected = []int64{29999, 30000}
						}
						if len(rows) != len(expected) {
							t.Fatalf("summary evidence=%d want=%d", len(rows), len(expected))
						}
						for i, row := range rows {
							if row.OrganizationID != org || row.ChangeID != args.ChangeID || row.Sequence != expected[i] || row.ActorPrincipalID == actor {
								t.Fatalf("summary lost current actor/tenant identity: %+v", row)
							}
						}
						var visited float64
						for _, statement := range []struct {
							sql  string
							args []any
						}{{headSQL, headArgs}, {probe.statement, append([]any(nil), probe.args...)}} {
							var raw []byte
							if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement.sql, statement.args...).Scan(&raw); err != nil {
								t.Fatal(err)
							}
							var plans []struct{ Plan searchPlanNode }
							if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
								t.Fatalf("decode review lookup plan: %v", err)
							}
							visited += currentReviewLookupVisits(plans[0].Plan)
							t.Logf("current actors=%d historical reviews=30000 cumulative visits=%.0f plan=%s", actors, visited, raw)
						}
						if visited > 32 {
							t.Errorf("current review lookup visited %.0f rows for2 current decisions", visited)
						}
						args.OrganizationID = "org_foreign"
						foreign, err := queries.RecordCurrentChangeReviewSequences(t.Context(), args)
						if err != nil || len(foreign) != 0 {
							t.Fatalf("foreign summary rows=%d error=%v", len(foreign), err)
						}
					})
				}
				if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
					_, _, err := r.(eventstore.ChangeReviewEvidenceReader).ChangeReviewEvidence(t.Context(), fmt.Sprintf("change_current_%d", actors), fmt.Sprintf("version_current_%d", actors), 30000, nil, "actor_example")
					if !errors.Is(err, corestore.ErrRevisionConflict) {
						return fmt.Errorf("historical snapshot did not conflict: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func currentReviewLookupVisits(node searchPlanNode) float64 {
	var count float64
	if node.Relation == "change_evidence" || node.Relation == "current_change_reviews" {
		count = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
	}
	for _, child := range node.Plans {
		count += currentReviewLookupVisits(child)
	}
	return count
}

func TestMachineReviewLookupsSeekPastHistoryAndLandingRows(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		f := seedHistory(t, repository, org, "machine_plan")
		for index, kind := range []string{"validator", "implementation", "security_audit"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence
(organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_landing,review_session_known)
SELECT $1,$2,$4+n+1,$3,'review','worker',$5||'_'||n::text,CASE WHEN n%2=0 THEN 'commented' ELSE 'approved' END,
$6::timestamptz,$7,$8,1,1,$5,($5='security_audit' AND n>20000),true FROM generate_series(1,30000)n`, org, f.change.ID, f.version.ID, index*30000, kind, historyAt(), f.evidence.PayloadObjectKey, f.evidence.PayloadSHA256); err != nil {
				t.Fatal(err)
			}
		}
	}
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if _, err := admin.ExecContext(t.Context(), `ANALYZE change_evidence; ANALYZE current_change_reviews`); err != nil {
				t.Fatal(err)
			}
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, test := range []struct {
				through int64
				missing bool
				want    []int64
			}{
				{through: 1}, {through: 4, want: []int64{4}}, {through: 30001, want: []int64{30001}},
				{through: 60001, want: []int64{30001, 60001}}, {through: 90001, want: []int64{30001, 60001, 80001}}, {through: 90001, missing: true},
			} {
				t.Run(fmt.Sprintf("%s/analyzed=%t/through=%d/missing=%t", org, analyzed, test.through, test.missing), func(t *testing.T) {
					tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
						t.Fatal(err)
					}
					probe := &searchPlanProbe{DBTX: tx}
					change := "change_machine_plan"
					if test.missing {
						change = "change_missing"
					}
					got, err := pgquery.New(probe).RecordCurrentChangeReviewSequences(t.Context(), pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: change, VersionID: "version_machine_plan", ReviewingActor: "example-owner", Through: test.through})
					if err != nil || !slices.Equal(got, test.want) {
						t.Fatalf("machine prefix=%v want=%v error=%v", got, test.want, err)
					}
					var raw []byte
					if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.statement, probe.args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan searchPlanNode }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("decode machine head plan: %v", err)
					}
					visited := currentReviewLookupVisits(plans[0].Plan)
					t.Logf("machine reviews=90000 visited=%.0f plan=%s", visited, raw)
					if visited > 32 {
						t.Errorf("fixed3-role lookup visited %.0f rows", visited)
					}
				})
			}
		}
	}
}

type currentReviewReadProbe struct {
	attachment.DetailStore
	before func(context.Context) error
}

func (p currentReviewReadProbe) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := p.before(ctx); err != nil {
		return nil, err
	}
	return p.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestCurrentMachineWaitSupersedesAnAlreadyCertifiedPass(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, kind := range []string{"validator", "implementation", "security_audit"} {
			t.Run(string(org)+"/"+kind, func(t *testing.T) {
				f := seedHistory(t, repository, org, kind)
				if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET current_version_id=$3 WHERE organization_id=$1 AND id=$2`, org, f.change.ID, f.version.ID); err != nil {
					t.Fatal(err)
				}
				reviewNumber := int64(0)
				appendReview := func(decision string, landing bool) {
					t.Helper()
					reviewNumber++
					verdict := "pass"
					if decision == "commented" {
						verdict = "wait"
					}
					review := tracker.ChangeReview{ID: fmt.Sprintf("review_%s_%d", kind, reviewNumber), VersionID: f.version.ID, Decision: decision, Actor: historyActor(), CreatedAt: historyAt()}
					result := &gate.ValidatorResult{VersionID: f.version.ID, SessionID: 1, Verdict: verdict}
					switch kind {
					case "validator":
						review.Validator = result
					case "implementation":
						review.ImplementationReview = result
					case "security_audit":
						review.SecurityAudit = &tracker.SecurityAuditReview{VersionID: f.version.ID, SessionID: 1, Landing: landing}
					}
					row, err := f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.change.ID, Kind: "review"}, historyJSON(t, review))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
						t.Fatal(err)
					}
					if _, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil {
						t.Fatal(err)
					}
				}
				assertHead := func(sequence int64, decision string) {
					t.Helper()
					tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, org); err != nil {
						t.Fatal(err)
					}
					sequences, err := pgquery.New(tx).RecordCurrentChangeReviewSequences(t.Context(), pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: f.change.ID, VersionID: f.version.ID, ReviewingActor: "example-owner", Through: reviewNumber + 1})
					if err != nil {
						t.Fatal(err)
					}
					if len(sequences) != 1 || sequences[0] != sequence {
						t.Fatalf("%s latest %s review lost: selected=%v want=%d", kind, decision, sequences, sequence)
					}
					if kind == "security_audit" {
						return
					}
					var evidence []eventstore.Evidence
					if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
						var err error
						evidence, _, err = r.(eventstore.ChangeReviewEvidenceReader).ChangeReviewEvidence(t.Context(), f.change.ID, f.version.ID, sequence, nil, "example-owner")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					version := tracker.ChangeVersion{ID: f.version.ID, ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: "example-policy"}, Policy: policy.Descriptor{ID: "example-policy"}, ReviewPolicy: tracker.ChangeReviewPolicy{ID: "example-review-policy"}}
					if kind == "validator" {
						version.Policy.Gates.Validator = true
					} else {
						version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
					}
					detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: f.version.ID}, Versions: []tracker.ChangeVersion{version}}
					for _, row := range evidence {
						raw, err := f.content.Evidence(t.Context(), row)
						if err != nil {
							t.Fatal(err)
						}
						review, err := eventstore.DecodeChangeReview(row, raw)
						if err != nil {
							t.Fatal(err)
						}
						detail.Reviews = append(detail.Reviews, review)
					}
					summary := changerequest.SummarizeCurrentVersion(detail, version.PolicyID, version.ReviewPolicy.ID, historyAt(), false)
					if (summary.Status == "reviewed") != (decision == "approved") {
						t.Fatalf("%s %s inherited wrong approval: %+v", kind, decision, summary)
					}
				}
				appendReview("approved", false)
				assertHead(2, "approved")
				appendReview("commented", false)
				assertHead(3, "commented")
				appendReview("approved", false)
				assertHead(4, "approved")
				if kind == "security_audit" {
					appendReview("commented", true)
					assertHead(4, "approved")
				}
			})
		}
	}
}

func TestCurrentChangeReviewsBackfillPreservesConcurrentAndLateEvidence(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	fixtures := map[tracker.OrganizationID]historyFixture{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		f := seedHistory(t, repository, org, "backfill")
		change, err := f.content.Change(t.Context(), f.change)
		if err != nil {
			t.Fatal(err)
		}
		change.CurrentVersion, change.Revision = f.version.ID, 2
		updated, err := f.content.PrepareChange(t.Context(), org, historyJSON(t, change))
		if err != nil {
			t.Fatal(err)
		}
		updated.Revision = 1
		if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.UpdateChange(t.Context(), updated); return err }); err != nil {
			t.Fatal(err)
		}
		for i := range 301 {
			decision := "approved"
			if i%2 == 1 {
				decision = "changes_requested"
			}
			review := tracker.ChangeReview{ID: fmt.Sprintf("review_%d", i), VersionID: f.version.ID, Decision: decision, Actor: tracker.Actor{Kind: "human", PrincipalID: fmt.Sprintf("reviewer_%d", i%2)}, CreatedAt: historyAt()}
			row, err := f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.change.ID, Kind: "review"}, historyJSON(t, review))
			if err != nil {
				t.Fatal(err)
			}
			if i == 198 {
				row.ReviewKind = ""
				row.ReviewSessionKnown = false
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
				t.Fatal(err)
			}
		}
		var derived int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM current_change_reviews WHERE organization_id=$1 AND change_id=$2`, org, f.change.ID).Scan(&derived); err != nil || derived != 0 {
			t.Fatalf("request maintained derived current reviews: count=%d error=%v", derived, err)
		}
		fixtures[org] = f
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			f := fixtures[org]
			read := func(through int64) ([]eventstore.Evidence, error) {
				var rows []eventstore.Evidence
				err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
					var err error
					rows, _, err = r.(eventstore.ChangeReviewEvidenceReader).ChangeReviewEvidence(t.Context(), f.change.ID, f.version.ID, through, nil, "new_reviewer")
					return err
				})
				return rows, err
			}
			if _, err := read(302); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unbuilt current reviews accepted: %v", err)
			}
			prepare := func(id, actor, decision string, sequence int64, known bool) eventstore.Evidence {
				t.Helper()
				row, err := f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.change.ID, Kind: "review", Sequence: sequence, SourceKey: new(id)}, historyJSON(t, tracker.ChangeReview{ID: id, VersionID: f.version.ID, Decision: decision, Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: historyAt()}))
				if err != nil {
					t.Fatal(err)
				}
				if !known {
					row.ReviewKind = ""
					row.ReviewSessionKnown = false
				}
				return row
			}
			appendRow := func(ctx context.Context, row eventstore.Evidence) error {
				return repository.WriteRecords(ctx, org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(ctx, row); return err })
			}
			future := prepare("review_future", "reviewer_1", "changes_requested", 0, true)
			var once sync.Once
			var appendErr error
			probe := currentReviewReadProbe{DetailStore: f.objects, before: func(ctx context.Context) error {
				tx, err := admin.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				var id string
				err = tx.QueryRowContext(ctx, `SELECT id FROM change_requests WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, org, f.change.ID).Scan(&id)
				err = errors.Join(err, tx.Rollback())
				if err != nil {
					return fmt.Errorf("archive hydration held Change lock: %w", err)
				}
				once.Do(func() { appendErr = appendRow(ctx, future) })
				return appendErr
			}}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, eventstore.NewContent(probe), 200); err != nil || n != 200 {
				t.Fatalf("first current review page=%d error=%v", n, err)
			}
			if _, err := read(303); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("partial current reviews accepted: %v", err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 103 {
				t.Fatalf("final current review page=%d error=%v", n, err)
			}
			rows, err := read(303)
			if err != nil || len(rows) != 2 || rows[0].Sequence != 302 || rows[1].Sequence != 303 {
				t.Fatalf("forward review lost during backfill: rows=%+v error=%v", rows, err)
			}
			var before, after eventstore.Change
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				before, err = r.Change(t.Context(), f.change.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := appendRow(t.Context(), future); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				after, err = r.Change(t.Context(), f.change.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("review replay changed current-review source metadata")
			}
			replayedRows, err := read(303)
			if err != nil || !reflect.DeepEqual(rows, replayedRows) {
				t.Fatalf("review replay changed derived heads: rows=%+v error=%v", replayedRows, err)
			}
			sparse := prepare("review_sparse", "reviewer_1", "changes_requested", 500, true)
			if err := appendRow(t.Context(), sparse); err != nil {
				t.Fatal(err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 1 {
				t.Fatalf("sparse source page=%d error=%v", n, err)
			}
			late := prepare("review_late", "reviewer_0", "approved", 400, true)
			if err := appendRow(t.Context(), late); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				after, err = r.Change(t.Context(), f.change.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if after.EvidenceSequence != 500 || after.ReviewSummarySequence != 399 || after.ReviewEvidenceRevision != before.ReviewEvidenceRevision+1 {
				t.Fatalf("late source did not advance its independent revision: %+v", after)
			}
			if _, err := read(500); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("late known review was treated as complete: %v", err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 2 {
				t.Fatalf("late known source page=%d error=%v", n, err)
			}
			rows, err = read(500)
			if err != nil || len(rows) != 2 || rows[0].Sequence != 400 || rows[1].Sequence != 500 {
				t.Fatalf("late review lost: rows=%+v error=%v", rows, err)
			}
			unknown := prepare("review_legacy", "reviewer_0", "approved", 450, false)
			if err := appendRow(t.Context(), unknown); err != nil {
				t.Fatal(err)
			}
			if _, err := read(500); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("late unprojected review was treated as complete: %v", err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 1 {
				t.Fatalf("legacy source page=%d error=%v", n, err)
			}
			if _, err := read(500); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unfinished suffix accepted: %v", err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 1 {
				t.Fatalf("legacy suffix page=%d error=%v", n, err)
			}
			rows, err = read(500)
			if err != nil || len(rows) != 2 || rows[0].Sequence != 450 || rows[1].Sequence != 500 {
				t.Fatalf("legacy review not recovered: rows=%+v error=%v", rows, err)
			}
			for _, test := range []struct {
				name  string
				tip   int64
				late  int64
				known bool
			}{
				{name: "competing backfill", tip: 600},
				{name: "late known insertion", tip: 700, late: 650, known: true},
				{name: "late unknown insertion", tip: 800, late: 750},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := appendRow(t.Context(), prepare(fmt.Sprintf("tip_%d", test.tip), "reviewer_0", "approved", test.tip, false)); err != nil {
						t.Fatal(err)
					}
					interleaved := false
					probe := currentReviewReadProbe{DetailStore: f.objects, before: func(ctx context.Context) error {
						if interleaved {
							return nil
						}
						interleaved = true
						if test.late != 0 {
							return appendRow(ctx, prepare(fmt.Sprintf("late_%d", test.late), "reviewer_1", "changes_requested", test.late, test.known))
						}
						_, err := repository.BackfillCurrentChangeReviews(ctx, org, f.content, 200)
						return err
					}}
					if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, eventstore.NewContent(probe), 200); n != 0 || !errors.Is(err, corestore.ErrRevisionConflict) || !interleaved {
						t.Fatalf("stale backfill committed: rows=%d interleaved=%t error=%v", n, interleaved, err)
					}
					if test.late != 0 {
						if _, err := read(test.tip); !errors.Is(err, corestore.ErrRevisionConflict) {
							t.Fatalf("late insertion left stale coverage certified: %v", err)
						}
						if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != map[bool]int{true: 2, false: 1}[test.known] {
							t.Fatalf("late insertion rebuild rows=%d error=%v", n, err)
						}
						if !test.known {
							if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.content, 200); err != nil || n != 1 {
								t.Fatalf("late insertion suffix rows=%d error=%v", n, err)
							}
						}
					}
					rows, err := read(test.tip)
					if err != nil || len(rows) != 2 || rows[1].Sequence != test.tip || test.late != 0 && rows[0].Sequence != test.late {
						t.Fatalf("interleaved evidence lost: rows=%+v error=%v", rows, err)
					}
				})
			}
		})
	}
}

func TestMachineReviewMigrationRejectsAnIndexThatIncludesLandingRows(t *testing.T) {
	admin := template.Database(t)
	admin.SetMaxOpenConns(1)
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261104090048); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE INDEX change_current_machine_reviews_idx ON change_evidence(organization_id,change_id,version_id,review_kind,sequence DESC) WHERE kind='review'`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Up(t.Context())
	var refused *pq.Error
	if !errors.As(err, &refused) || refused.Code != "P0001" {
		t.Fatalf("wrong machine review predicate was accepted: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY change_current_machine_reviews_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261104090048); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := admin.QueryRowContext(t.Context(), `SELECT to_regclass('public.change_current_machine_reviews_idx') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("machine index rollback exists=%t error=%v", exists, err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentReviewMigrationPreservesSourceIdentityOnRetry(t *testing.T) {
	admin := template.Database(t)
	admin.SetMaxOpenConns(1)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		seedHistory(t, repository, org, "migration")
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE UNIQUE INDEX change_evidence_source_identity_restore_idx ON change_evidence(organization_id,change_id,kind,source_key) NULLS NOT DISTINCT`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.DownTo(t.Context(), 20261104042809)
	var nullSemantics *pq.Error
	if !errors.As(err, &nullSemantics) || nullSemantics.Code != "P0001" {
		t.Fatalf("NULLS NOT DISTINCT restore index was not refused: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY change_evidence_source_identity_restore_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(t.Context(), 20261104042809); err != nil {
		t.Fatal(err)
	}
	check := func(phase int) {
		t.Helper()
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			_, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
    SELECT organization_id,change_id,$3,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=1`, org, "change_migration", int64(1000+phase*10))
			var duplicate *pq.Error
			if !errors.As(err, &duplicate) || duplicate.Code != "23505" {
				t.Fatalf("nonnull source identity phase=%d org=%s error=%v", phase, org, err)
			}
			for i := range 2 {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
     SELECT organization_id,change_id,$3,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=1`, org, "change_migration", int64(1001+phase*10+i)); err != nil {
					t.Fatalf("null source identity phase=%d org=%s error=%v", phase, org, err)
				}
			}
		}
	}
	check(0)
	if _, err := admin.ExecContext(t.Context(), `CREATE UNIQUE INDEX change_evidence_source_identity_idx ON change_evidence(change_id,organization_id,kind,source_key) WHERE source_key IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Up(t.Context())
	var refused *pq.Error
	if !errors.As(err, &refused) || refused.Code != "P0001" {
		t.Fatalf("wrong identity index was not refused by catalog validation: %v", err)
	}
	var preserved bool
	if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='public.change_evidence'::regclass AND conname='change_evidence_organization_id_change_id_kind_source_key_key')`).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("failed migration dropped existing uniqueness: preserved=%v error=%v", preserved, err)
	}
	if _, err := admin.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY change_evidence_source_identity_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := provider.DownTo(t.Context(), 20261104042809); err != nil {
		t.Fatal(err)
	}
	check(2)
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(3)
}
