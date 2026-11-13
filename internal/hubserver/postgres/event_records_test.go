package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func historyEventForItem(t *testing.T, c *eventstore.Content, org tracker.OrganizationID, id string, item tracker.NativeWorkItemID) eventstore.Event {
	t.Helper()
	value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: item, Type: "issue.edited", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: pgstoretest.HistoryAt(), Data: tracker.CollaborationData{Revision: 2, Fields: []string{"title"}, Reason: "Example reason"}}
	row, err := c.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestEventChangeTenantRoundTrip(t *testing.T) {
	t.Run("frozen version landing survives later current source and replay", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		for _, org := range []tracker.OrganizationID{"org_frozen_first", "org_frozen_second"} {
			pgstoretest.SeedRepository(t, repository, org)
			f := pgstoretest.SeedHistory(t, repository, org, "first")
			value, err := f.Content.Change(t.Context(), f.Change)
			if err != nil {
				t.Fatal(err)
			}
			value.CurrentVersion = f.Version.ID
			value.Landed = &tracker.ChangeLanding{VersionID: f.Version.ID, HeadSHA: f.Version.HeadSHA, MergeSHA: strings.Repeat("a", 40), BaseRef: "main", Method: "merge", Actor: pgstoretest.HistoryActor(), LandedAt: pgstoretest.HistoryAt().Add(time.Minute), Quality: &tracker.LandingQualitySnapshot{IssueRevision: 1, IssueBody: "Original frozen contract", Criteria: []string{"Original criterion"}}}
			prepared, err := f.Content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := eventstore.VersionLandingFromChange(prepared)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
				if _, err := w.UpdateChange(t.Context(), prepared); err != nil {
					return err
				}
				return w.RecordVersionLanding(t.Context(), frozen)
			}); err != nil {
				t.Fatal(err)
			}
			later, err := f.Content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_later", ChangeID: f.Change.ID, Number: 2, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "later-head", BaseSHA: "base", Repository: "owner/repository", PolicyID: f.Version.PolicyRevisionID}, Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt().Add(2 * time.Minute)}))
			if err != nil {
				t.Fatal(err)
			}
			laterLanding := tracker.ChangeLanding{VersionID: later.ID, HeadSHA: later.HeadSHA, MergeSHA: strings.Repeat("b", 40), BaseRef: "main", Method: "merge", Actor: pgstoretest.HistoryActor(), LandedAt: pgstoretest.HistoryAt().Add(3 * time.Minute), Quality: &tracker.LandingQualitySnapshot{IssueRevision: 2, IssueBody: "Later frozen contract", Criteria: []string{"Later criterion"}}}
			laterFrozen, err := f.Content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", ChangeID: f.Change.ID, VersionID: later.ID}, laterLanding)
			if err != nil {
				t.Fatal(err)
			}
			value.CurrentVersion = later.ID
			value.Landed = &laterLanding
			value.Revision = 2
			value.UpdatedAt = pgstoretest.HistoryAt().Add(3 * time.Minute)
			next, err := f.Content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
				if _, err := w.CreateVersion(t.Context(), later); err != nil {
					return err
				}
				if _, err := w.UpdateChange(t.Context(), next); err != nil {
					return err
				}
				return w.RecordVersionLanding(t.Context(), laterFrozen)
			}); err != nil {
				t.Fatal(err)
			}
			var firstRow, lastRow eventstore.VersionLanding
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				owner := r.(eventstore.VersionLandingReader)
				var err error
				firstRow, err = owner.VersionLanding(t.Context(), f.Change.ID, f.Version.ID)
				if err != nil {
					return err
				}
				lastRow, err = owner.VersionLanding(t.Context(), f.Change.ID, later.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			probe := pgstoretest.ReportObjectReadProbe{DetailStore: f.Objects, Check: func() {
				if held := repository.Stats().AcquiredConns(); held != 0 {
					t.Fatalf("frozen landing object read held %d database connections", held)
				}
			}}
			content := eventstore.NewContent(probe)
			original, err := content.VersionLanding(t.Context(), firstRow)
			if err != nil || original.VersionID != f.Version.ID || original.Quality == nil || original.Quality.IssueBody != "Original frozen contract" || len(original.Quality.Criteria) != 1 || original.Quality.Criteria[0] != "Original criterion" {
				t.Fatalf("original version landing=%+v error=%v", original, err)
			}
			newer, err := content.VersionLanding(t.Context(), lastRow)
			if err != nil || newer.VersionID != later.ID || newer.Quality == nil || newer.Quality.IssueBody != "Later frozen contract" {
				t.Fatalf("later version landing=%+v error=%v", newer, err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
				replayed, err := w.CreateVersion(t.Context(), f.Version)
				if err != nil {
					return err
				}
				if replayed.FrozenLanding == nil || replayed.FrozenLanding.PayloadObjectKey != firstRow.PayloadObjectKey {
					t.Fatalf("version replay discarded frozen metadata: %+v", replayed)
				}
				return w.RecordVersionLanding(t.Context(), firstRow)
			}); err != nil {
				t.Fatal(err)
			}
			unlanded, err := f.Content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_unlanded", ChangeID: f.Change.ID, Number: 3, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "unlanded-head", BaseSHA: "base", Repository: "owner/repository", PolicyID: f.Version.PolicyRevisionID}, Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt().Add(4 * time.Minute)}))
			if err != nil {
				t.Fatal(err)
			}
			unlanded.LandingHistoryKnown = false
			if err := repository.WriteRecords(t.Context(), org, corestore.Background, func(w eventstore.Writer) error {
				if _, err := w.CreateVersion(t.Context(), unlanded); err != nil {
					return err
				}
				for range 2 {
					if err := w.ConfirmVersionUnlanded(t.Context(), unlanded.ChangeID, unlanded.ID, unlanded.HeadSHA); err != nil {
						return err
					}
				}
				stored, err := w.Version(t.Context(), unlanded.ChangeID, unlanded.ID)
				if err != nil {
					return err
				}
				if !stored.LandingHistoryKnown || stored.FrozenLanding != nil {
					t.Fatalf("confirmed unlanded history=%+v", stored)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var historicalHead, historicalTail, currentPage eventstore.AnalyticsLandingPage
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(eventstore.AnalyticsLandingReader)
				query := eventstore.AnalyticsLandingQuery{ProjectID: "prj_first", Principal: "example", From: pgstoretest.HistoryAt(), To: pgstoretest.HistoryAt().Add(time.Hour), Limit: 1, AllVersions: true}
				var err error
				historicalHead, err = owner.ReadAnalyticsLandingPage(t.Context(), query)
				if err != nil {
					return err
				}
				query.After = historicalHead.Next
				historicalTail, err = owner.ReadAnalyticsLandingPage(t.Context(), query)
				if err != nil {
					return err
				}
				query.AllVersions = false
				query.After = eventstore.AnalyticsLandingCursor{}
				currentPage, err = owner.ReadAnalyticsLandingPage(t.Context(), query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(historicalHead.Items) != 1 || historicalHead.Items[0].VersionID != f.Version.ID || historicalHead.Next.VersionID != f.Version.ID || !historicalHead.Partial || historicalHead.HistoryUnknown || len(historicalTail.Items) != 1 || historicalTail.Items[0].VersionID != later.ID || historicalTail.Next.ChangeID != "" || !historicalTail.Partial {
				t.Fatalf("historical landed population head=%+v tail=%+v", historicalHead, historicalTail)
			}
			if len(currentPage.Items) != 1 || currentPage.Items[0].VersionID != later.ID || currentPage.Partial || currentPage.Next.ChangeID != "" {
				t.Fatalf("current landing population=%+v", currentPage)
			}
			changed := firstRow
			changed.MergeSHA = strings.Repeat("c", 40)
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { return w.RecordVersionLanding(t.Context(), changed) }); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("frozen landing replacement=%v", err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_versions(organization_id,id,change_id,number,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id) SELECT organization_id,'version_wrong_project',change_id,9999,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,'prj_second' FROM change_versions WHERE organization_id=$1 AND id=$2`, org, f.Version.ID); err == nil {
				t.Fatal("version accepted mismatched project")
			} else {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23503" || failure.ConstraintName != "change_versions_project_identity" {
					t.Fatalf("version project identity=%v", err)
				}
			}
			for _, statement := range []string{`UPDATE change_versions SET landed_merge_sha=repeat('c',40) WHERE organization_id=$1 AND id=$2`, `UPDATE change_versions SET landing_history_known=false WHERE organization_id=$1 AND id=$2`, `UPDATE change_versions SET project_id='prj_second' WHERE organization_id=$1 AND id=$2`} {
				if _, err := admin.ExecContext(t.Context(), statement, org, f.Version.ID); err == nil {
					t.Fatal("frozen landing projection accepted mutation")
				} else {
					var failure *pgconn.PgError
					if !errors.As(err, &failure) || failure.Code != "23514" {
						t.Fatalf("frozen landing mutation=%v", err)
					}
				}
			}
			tampered := firstRow
			tampered.VersionID = later.ID
			if _, err := content.VersionLanding(t.Context(), tampered); !errors.Is(err, artifact.ErrIntegrity) {
				t.Fatalf("frozen landing identity mismatch=%v", err)
			}
		}
	})
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, org)
		for _, suffix := range []string{"first", "second"} {
			t.Run(string(org)+"/"+suffix, func(t *testing.T) {
				f := pgstoretest.SeedHistory(t, r, org, suffix)
				if f.Event.Sequence < 1 || f.Evidence.Sequence != 1 {
					t.Fatalf("missing sequences: event=%d evidence=%d", f.Event.Sequence, f.Evidence.Sequence)
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
					if events, err = reader.Events(t.Context(), "prj_first", "wi_first_1", f.Event.Sequence-1, 1); err != nil {
						return err
					}
					if snapshots, err = reader.CollaborationVersion(t.Context(), f.Snapshot.RecordID, 2); err != nil {
						return err
					}
					if change, err = reader.Change(t.Context(), f.Change.ID); err != nil {
						return err
					}
					after := ""
					if suffix == "second" {
						after = "change_first"
					}
					if changes, err = reader.Changes(t.Context(), "prj_first", f.Link.WorkItemID, after, 1); err != nil {
						return err
					}
					if versions, err = reader.Versions(t.Context(), f.Change.ID, 0, 1); err != nil {
						return err
					}
					if evidence, err = reader.Evidence(t.Context(), f.Change.ID, 0, 1); err != nil {
						return err
					}
					if receipt, err = reader.Receipt(t.Context(), f.Receipt.ActorID, f.Receipt.Operation, f.Receipt.CommandKey); err != nil {
						return err
					}
					links, err = reader.Links(t.Context(), f.Change.ID, "", 1)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct {
					name      string
					got, want any
				}{
					{"events", events, []eventstore.Event{f.Event}},
					{"snapshot", snapshots, f.Snapshot},
					{"change", change, f.Change},
					{"changes", changes, []eventstore.Change{f.Change}},
					{"versions", versions, []eventstore.Version{f.Version}},
					{"evidence", evidence, []eventstore.Evidence{f.Evidence}},
					{"links", links, []eventstore.Link{f.Link}},
					{"receipt", receipt, f.Receipt},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if !reflect.DeepEqual(tc.got, tc.want) {
							t.Fatalf("round trip mismatch: got %+v want %+v", tc.got, tc.want)
						}
					})
				}
				response, err := f.Content.Receipt(t.Context(), receipt)
				if err != nil || string(response) != `{"change_id":"`+f.Change.ID+`"}` {
					t.Fatalf("receipt response=%s error=%v", response, err)
				}
				event, err := f.Content.Event(t.Context(), events[0])
				if err != nil || event.Data.Reason != "Example reason" || event.Actor.DisplayName != "Example actor" || event.AggregateSequence != f.Event.Sequence {
					t.Fatalf("event payload=%+v error=%v", event, err)
				}
				hydrated, err := f.Content.Change(t.Context(), change)
				if err != nil || hydrated.Body != "Change body" || len(hydrated.LinkedIssues) != 2 {
					t.Fatalf("change payload=%+v error=%v", hydrated, err)
				}
				version, err := f.Content.Version(t.Context(), versions[0])
				if err != nil || version.External == nil || version.External.ID != "external" || version.AttemptID != "attempt" {
					t.Fatalf("version payload=%+v error=%v", version, err)
				}
				wantSource := tracker.ChangeSource{Format: "git-bundle", BaseSHA: "base", HeadSHA: "head", BundleSHA256: artifact.Digest(bytes.Repeat([]byte(string(org)+suffix), 123)[:123]), DiffSHA256: artifact.Digest([]byte("diff")), Bytes: 123}
				if version.Source == nil || *version.Source != wantSource {
					t.Fatalf("portable source manifest=%+v want=%+v", version.Source, wantSource)
				}
				raw, err := f.Content.Evidence(t.Context(), evidence[0])
				var discussion tracker.ChangeDiscussion
				if err != nil || json.Unmarshal(raw, &discussion) != nil || discussion.Body != "Evidence body" || discussion.Provenance.AuthorID != "author" {
					t.Fatalf("evidence payload=%s error=%v", raw, err)
				}
				raw, err = f.Content.CollaborationVersion(t.Context(), snapshots)
				var snapshot tracker.ChangeRequest
				if err != nil || json.Unmarshal(raw, &snapshot) != nil || snapshot.Body != "Change body" {
					t.Fatalf("snapshot payload=%s error=%v", raw, err)
				}
			})
		}
	}
}

func TestLatestRunEventUsesTypedAttemptIndex(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, org)
		f := pgstoretest.SeedHistory(t, r, org, "latest-run-event")
		content := eventstore.NewContent(f.Objects)
		appendRunEvent := func(id, attempt string) eventstore.Event {
			t.Helper()
			run := tracker.NativeRunData{AttemptID: attempt, RunID: "run_" + attempt, LeaseID: tracker.LeaseID("lease_" + attempt), FencingToken: 1, Sequence: 1}
			if attempt == "attempt_target" {
				run.Runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Waiting: true, ChangeID: "change_target", VersionID: "version_target", HeadSHA: "head_target"}}
			}
			value := tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateType: "issue", AggregateID: "wi_first_1", Type: "run.observed", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: pgstoretest.HistoryAt(), Data: tracker.CollaborationData{Run: &run}}
			record, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, value))
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
	t.Run("analytics workflow and evidence seek before residuals", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		capture := &analyticsQualityQueryCapture{}
		observed := pgstoretest.TracedFeaturePool(t, pool, capture)
		repository, err := postgres.NewRepository(t.Context(), observed, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		runtime, err := postgres.NewRuntime(t.Context(), observed, observed)
		if err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_quality_first", "org_quality_second"} {
			pgstoretest.SeedRepository(t, repository, org)
			f := pgstoretest.SeedHistory(t, repository, org, "analytics_quality")
			base := pgstoretest.HistoryAt()
			for _, statement := range []string{
				`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,'prj_first','wi_first_1','workflow-'||lpad(n::text,4,'0'),n+2,'workflow.transitioned',1,'human','example','Todo','In Progress',$2,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(0,1000)n`,
				`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,'prj_first','wi_first_1','run-noise-'||lpad(n::text,5,'0'),n+1003,'run.observed',1,'runner','example',$2,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(0,9999)n`,
			} {
				if _, err := admin.ExecContext(t.Context(), statement, org, base); err != nil {
					t.Fatal(err)
				}
			}
			review := tracker.ChangeReview{ID: "review", VersionID: f.Version.ID, Actor: pgstoretest.HistoryActor(), Decision: "changes_requested", CreatedAt: base.Add(time.Minute)}
			evidence, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "review"}, pgstoretest.HistoryJSON(t, review))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(quality_at,organization_id,project_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_session_known) SELECT $4::timestamptz,$1,'prj_first',$2,n+2,$3,'review','human','example','changes_requested',$4,$5,$6,$7,$8,'human',true FROM generate_series(0,1000)n`, org, f.Change.ID, f.Version.ID, review.CreatedAt, evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(organization_id,project_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,'prj_first',$2,n+1003,$3,'check','human','example',$4,$5,$6,$7,$8 FROM generate_series(0,9999)n`, org, f.Change.ID, f.Version.ID, base, evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(quality_at,organization_id,project_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_session_known) SELECT $4::timestamptz-interval '1 day',$1,'prj_first',$2,n+20000,$3,'review','human','example','changes_requested',$4::timestamptz-interval '1 day',$5,$6,$7,$8,'human',true FROM generate_series(0,9999)n`, org, f.Change.ID, f.Version.ID, base, evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,native_id,number,title) SELECT $1,'prj_second','quality-noise-'||n,n+1000,'Unrelated item' FROM generate_series(1,10000)n`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_requests(organization_id,project_id,work_item_id,id,title,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,'prj_second','wi_second_1','quality-noise-change-'||n,'Unrelated change',$2,$3,$4,$5 FROM generate_series(1,10000)n`, org, f.Change.PayloadObjectKey, f.Change.PayloadSHA256, f.Change.PayloadBytes, f.Change.PayloadContentBytes); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"collaboration_events", "change_evidence", "issues", "change_requests"} {
				if _, err := admin.ExecContext(t.Context(), "ANALYZE "+table); err != nil {
					t.Fatal(err)
				}
			}
			plan := func(family, relation string, budget float64) {
				t.Helper()
				capture.mu.Lock()
				statement, args := capture.sql[family], append([]any(nil), capture.args[family]...)
				capture.mu.Unlock()
				if statement == "" {
					t.Fatalf("missing %s source query", family)
				}
				tx, err := runtime.Begin(t.Context(), org, pgtransport.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plans []struct{ Plan pgstoretest.SearchPlanNode }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatalf("%s plan error=%v", family, err)
				}
				visits := float64(0)
				indexed := false
				var servedIndex func(pgstoretest.SearchPlanNode) bool
				servedIndex = func(node pgstoretest.SearchPlanNode) bool {
					if node.Index != "" && node.Loops > 0 {
						return true
					}
					for _, child := range node.Plans {
						if servedIndex(child) {
							return true
						}
					}
					return false
				}
				metadata := map[string]float64{}
				var inspect func(pgstoretest.SearchPlanNode)
				inspect = func(node pgstoretest.SearchPlanNode) {
					if node.Relation == relation {
						visits += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
						indexed = indexed || servedIndex(node)
					}
					if node.Relation == "issues" || node.Relation == "change_requests" || node.Relation == "change_versions" {
						metadata[node.Relation] += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
					}
					for _, child := range node.Plans {
						inspect(child)
					}
				}
				inspect(plans[0].Plan)
				if metadata["issues"] > 1 || metadata["change_requests"] > 1 || metadata["change_versions"] > 3 {
					t.Fatalf("%s metadata point visits=%v plan=%s", family, metadata, raw)
				}
				if visits > budget || !indexed {
					t.Fatalf("%s source visits=%.0f budget=%.0f plan=%s", family, visits, budget, raw)
				}
			}
			workflow := eventstore.AnalyticsWorkflowQuery{ProjectID: "prj_first", Principal: "example", From: base, To: base.Add(time.Hour), Limit: 1000}
			readWorkflow := func() eventstore.AnalyticsWorkflowPage {
				t.Helper()
				var page eventstore.AnalyticsWorkflowPage
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(eventstore.AnalyticsWorkflowReader).ReadAnalyticsWorkflowPage(t.Context(), workflow)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			head := readWorkflow()
			plan("workflow", "collaboration_events", 1001)
			if len(head.Items) != 1000 || head.Items[0].Event.ID != "workflow-0000" || head.Next.ID != "workflow-0999" || !head.Partial {
				t.Fatalf("workflow head=%+v", head)
			}
			workflow.After = head.Next
			tail := readWorkflow()
			plan("workflow", "collaboration_events", 1001)
			if len(tail.Items) != 1 || tail.Items[0].Event.ID != "workflow-1000" || tail.Next.ID != "" || !tail.Partial {
				t.Fatalf("workflow tail=%+v", tail)
			}
			workflow.After = eventstore.AnalyticsWorkflowCursor{}
			workflow.Principal = "unrelated"
			hidden := readWorkflow()
			plan("workflow", "collaboration_events", 1001)
			if len(hidden.Items) != 0 || hidden.Next.ID != "workflow-0999" || !hidden.Partial {
				t.Fatalf("workflow visibility residual=%+v", hidden)
			}
			query := eventstore.AnalyticsEvidenceQuery{ProjectID: "prj_first", Principal: "example", From: base, To: base.Add(time.Hour), Limit: 1000}
			readEvidence := func() eventstore.AnalyticsEvidencePage {
				t.Helper()
				var page eventstore.AnalyticsEvidencePage
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(eventstore.AnalyticsEvidenceReader).ReadAnalyticsEvidencePage(t.Context(), query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			first := readEvidence()
			plan("evidence", "change_evidence", 1001)
			if len(first.Items) != 1000 || first.Items[0].Evidence.Sequence != 2 || first.Next.Sequence != 1001 || !first.Partial {
				t.Fatalf("evidence head rows=%d next=%+v partial=%v", len(first.Items), first.Next, first.Partial)
			}
			query.After = first.Next
			last := readEvidence()
			plan("evidence", "change_evidence", 1001)
			if len(last.Items) != 1 || last.Items[0].Evidence.Sequence != 1002 || last.Next.ChangeID != "" || !last.Partial {
				t.Fatalf("evidence tail=%+v", last)
			}
			query.After = eventstore.AnalyticsEvidenceCursor{}
			query.Principal = "unrelated"
			invisible := readEvidence()
			plan("evidence", "change_evidence", 1001)
			if len(invisible.Items) != 0 || invisible.Next.Sequence != 1001 || !invisible.Partial {
				t.Fatalf("evidence visibility residual=%+v", invisible)
			}
			query.Principal = "example"
			query.After = eventstore.AnalyticsEvidenceCursor{}
			query.From = base.Add(2 * time.Minute)
			quiet := readEvidence()
			plan("evidence", "change_evidence", 1001)
			if len(quiet.Items) != 0 || quiet.Partial || quiet.Next.ChangeID != "" {
				t.Fatalf("old evidence poisoned quiet window=%+v", quiet)
			}
			query.From = base
			versions := make([]eventstore.Version, 0, 1002)
			for number := int64(2); number <= 1003; number++ {
				created := base.Add(30 * time.Minute)
				if number == 1003 {
					created = base.Add(10 * time.Second)
				}
				version, err := f.Content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: fmt.Sprintf("asof-%04d", number), ChangeID: f.Change.ID, Number: number, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: fmt.Sprintf("head-%d", number), PolicyID: f.Version.PolicyRevisionID}, Actor: pgstoretest.HistoryActor(), CreatedAt: created}))
				if err != nil {
					t.Fatal(err)
				}
				versions = append(versions, version)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error {
				for _, version := range versions {
					if _, err := w.CreateVersion(t.Context(), version); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			pendingReview := review
			pendingReview.ID = "nonmonotonic-asof"
			pendingEvidence, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "review", Sequence: 11003}, pgstoretest.HistoryJSON(t, pendingReview))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(t.Context(), pendingEvidence); return err }); err != nil {
				t.Fatal(err)
			}
			query.Principal = "example"
			query.After = eventstore.AnalyticsEvidenceCursor{At: review.CreatedAt, Sequence: 11002, ChangeID: f.Change.ID}
			pending := readEvidence()
			if len(pending.Items) != 0 || !pending.Partial || pending.Next.Sequence != 11003 || pending.Next.VersionAfter != 1002 || pending.Next.VersionThrough != 1003 {
				t.Fatalf("as-of version frontier=%+v", pending)
			}
			query.After = pending.Next
			resolved := readEvidence()
			if len(resolved.Items) != 0 || !resolved.Partial || resolved.Next.ChangeID != "" {
				t.Fatalf("later-number earlier-time version was included=%+v", resolved)
			}
			drift, err := f.Content.PrepareVersion(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "asof-drift", ChangeID: f.Change.ID, Number: 1004, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "drift", PolicyID: f.Version.PolicyRevisionID}, Actor: pgstoretest.HistoryActor(), CreatedAt: base.Add(40 * time.Minute)}))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.CreateVersion(t.Context(), drift); return err }); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(eventstore.AnalyticsEvidenceReader).ReadAnalyticsEvidencePage(t.Context(), query)
				return err
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("changed version frontier error=%v", err)
			}

			discussion := tracker.ChangeDiscussion{ID: "observed-time", VersionID: f.Version.ID, Actor: pgstoretest.HistoryActor(), CreatedAt: base.Add(4 * time.Minute), Escape: &tracker.QualityEscape{OccurrenceID: strings.Repeat("d", 40), Kind: "revert", Cause: "missing_criterion", ObservedAt: base.Add(3 * time.Minute), EvidenceReference: "record", EvidenceQuote: "failure", BasisQuote: "contract"}}
			escapeRow, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "discussion", Sequence: 30001}, pgstoretest.HistoryJSON(t, discussion))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(t.Context(), escapeRow); return err }); err != nil {
				t.Fatal(err)
			}
			query.After = eventstore.AnalyticsEvidenceCursor{}
			query.From = base.Add(2 * time.Minute)
			for _, test := range []struct {
				to   time.Time
				want int
			}{{base.Add(3 * time.Minute), 0}, {base.Add(210 * time.Second), 0}, {base.Add(5 * time.Minute), 1}} {
				query.To = test.to
				page := readEvidence()
				plan("evidence", "change_evidence", 1001)
				if len(page.Items) != test.want || page.Partial || page.Next.ChangeID != "" {
					t.Fatalf("observed/as-of window to=%s page=%+v", test.to, page)
				}
				if test.want == 1 && (page.Items[0].Escape == nil || !page.Items[0].Escape.ObservedAt.Equal(base.Add(3*time.Minute))) {
					t.Fatalf("escape used creation time=%+v", page)
				}
			}

		}
	})
	t.Run("scheduler seek bounds outcomes and batches immediate predecessors", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		capture := &diagnosticsQueryCapture{}
		observedPool := pgstoretest.TracedFeaturePool(t, pool, capture)
		repository, err := postgres.NewRepository(t.Context(), observedPool, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		runtime, err := postgres.NewRuntime(t.Context(), observedPool, observedPool)
		if err != nil {
			t.Fatal(err)
		}
		base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		for _, org := range []tracker.OrganizationID{"org_scheduler_first", "org_scheduler_second"} {
			pgstoretest.SeedRepository(t, repository, org)
			for _, source := range []struct {
				project, item, prefix, outcome string
				count, sequence, step          int
				at                             time.Time
			}{{"prj_first", "wi_first_1", "skip", "skipped", 1001, 1, 2, base}, {"prj_first", "wi_first_1", "claim", "claimed", 1001, 2, 2, base.Add(time.Minute)}, {"prj_second", "wi_second_1", "other", "skipped", 10000, 1, 1, base}} {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,scheduler_outcome,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,$2,$3,$4||'-'||lpad(n::text,4,'0'),$7::integer+n*$8::integer,'scheduler.decision',1,$5,$6,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(0,$9::integer-1)n`, org, source.project, source.item, source.prefix, source.outcome, source.at, source.sequence, source.step, source.count); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,'prj_first','wi_first_2','aa-workflow-'||lpad(n::text,5,'0'),n+1,'workflow.transitioned',1,$2,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(0,9999)n`, org, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE collaboration_events"); err != nil {
				t.Fatal(err)
			}
			read := func(query eventstore.AnalyticsSchedulerQuery) eventstore.AnalyticsSchedulerPage {
				t.Helper()
				var page eventstore.AnalyticsSchedulerPage
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(eventstore.AnalyticsSchedulerReader).ReadAnalyticsSchedulerPage(t.Context(), query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			plan := func(predecessor bool) {
				t.Helper()
				capture.mu.Lock()
				statement, args := capture.scheduler, append([]any(nil), capture.schedulerArgs...)
				if predecessor {
					statement, args = capture.predecessor, append([]any(nil), capture.predecessorArgs...)
				}
				capture.mu.Unlock()
				tx, err := runtime.Begin(t.Context(), org, pgtransport.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var nodes []struct{ Plan pgstoretest.SearchPlanNode }
				if err := json.Unmarshal(raw, &nodes); err != nil || len(nodes) != 1 {
					t.Fatalf("scheduler plan error=%v", err)
				}
				visits, indexed := float64(0), false
				var inspect func(pgstoretest.SearchPlanNode)
				inspect = func(node pgstoretest.SearchPlanNode) {
					if node.Relation == "collaboration_events" {
						visits += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
						indexed = indexed || node.Index != "" && node.Loops > 0
					}
					for _, child := range node.Plans {
						inspect(child)
					}
				}
				inspect(nodes[0].Plan)
				if predecessor {
					if !indexed || visits > 1000 {
						t.Fatalf("scheduler predecessor history visits=%.0f plan=%s", visits, raw)
					}
				} else if !nodes[0].Plan.UsesIndex("collaboration_events_analytics_scheduler_seek") || visits > 1001 {
					t.Fatalf("scheduler source visits=%.0f plan=%s", visits, raw)
				}
			}
			query := eventstore.AnalyticsSchedulerQuery{ProjectID: "prj_first", Principal: "example", From: base, To: base.Add(time.Hour), Outcome: "skipped", Limit: 1000}
			head := read(query)
			plan(false)
			if len(head.Events) != 1000 || head.Events[0].ID != "skip-0000" || head.Next.ID != "skip-0999" || head.Examined != 1001 || !head.Partial || len(head.Prior) != 0 {
				t.Fatalf("scheduler head rows=%d examined=%d next=%+v", len(head.Events), head.Examined, head.Next)
			}
			query.After = head.Next
			tail := read(query)
			plan(false)
			if len(tail.Events) != 1 || tail.Events[0].ID != "skip-1000" || tail.Examined != 1 || !tail.Partial || tail.Next.ID != "" {
				t.Fatalf("scheduler tail=%+v", tail)
			}
			query.After = eventstore.AnalyticsSchedulerCursor{}
			query.Principal = "unrelated"
			hidden := read(query)
			if len(hidden.Events) != 0 || hidden.Examined != 1001 || hidden.Next.ID != "skip-0999" || !hidden.Partial {
				t.Fatalf("scheduler visibility residual=%+v", hidden)
			}
			query.Principal = "example"
			query.Outcome = "claimed"
			claims := read(query)
			plan(false)
			plan(true)
			if len(claims.Events) != 1000 || claims.Examined != 1001 || len(claims.Prior) != 1000 || claims.Next.ID != "claim-0999" || !claims.Partial {
				t.Fatalf("claimed population rows=%d prior=%d examined=%d next=%+v", len(claims.Events), len(claims.Prior), claims.Examined, claims.Next)
			}
			for _, event := range claims.Events {
				prior, ok := claims.Prior[event.ID]
				if !ok || prior.OrganizationID != org || prior.ProjectID != "prj_first" || prior.WorkItemID != event.WorkItemID || prior.Sequence != event.Sequence-1 || prior.ID != "skip-"+strings.TrimPrefix(event.ID, "claim-") {
					t.Fatalf("immediate predecessor claim=%+v prior=%+v", event, prior)
				}
			}
		}
	})
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	attempts := make([]string, 17)
	for i := range attempts {
		attempts[i] = fmt.Sprintf("attempt_%d", i+1)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, org)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events
			(organization_id,project_id,work_item_id,id,sequence,schema_version,actor_kind,actor_principal_id,actor_envelope_known,actor_via_key,actor_display_name,actor_agent,run_id,landing_waiting,landing_change_id,landing_version_id,landing_head_sha,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			SELECT $1,'prj_first','wi_first_1','event_'||n,n,1,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'runner' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'example-runner' ELSE '' END,
            n BETWEEN 1684 AND 1700,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'example-key' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'Example actor' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'example-agent' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'run_target' ELSE '' END,
            n BETWEEN 1684 AND 1700,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'change_target' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'version_target' ELSE '' END,
            CASE WHEN n BETWEEN 1684 AND 1700 THEN 'head_target' ELSE '' END,
			CASE WHEN n<=1700 THEN 'attempt_'||((n-1)%17+1) ELSE 'unrelated_'||n END,
			$2::timestamptz-n*interval '1 second',
			'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
			FROM generate_series(1,30000) n`, org, pgstoretest.HistoryAt()); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET event_sequence=30000 WHERE organization_id=$1 AND project_id='prj_first' AND native_id='wi_first_1'`, org); err != nil {
			t.Fatal(err)
		}

		f := pgstoretest.SeedHistory(t, r, org, "diagnostics-wait")
		event, err := f.Content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "diagnostics-merge-entry", OrganizationID: org, ProjectID: "prj_first", AggregateID: "wi_first_1", Type: "workflow.transitioned", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: pgstoretest.HistoryAt().Add(-2 * time.Hour), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Merging"}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET landed_version_id=$2,landed_at=$3,landed_head_sha=$4,landed_merge_sha=$5,landed_base_ref='main' WHERE organization_id=$1 AND id=$6`, org, f.Version.ID, pgstoretest.HistoryAt().Add(time.Hour), f.Version.HeadSHA, strings.Repeat("a", 40), f.Change.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
		t.Fatal(err)
	}
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				owner, ok := reader.(eventstore.RunEventBatchReader)
				if !ok {
					return errors.New("batched run-event owner unavailable")
				}
				events, err := owner.LatestRunEvents(t.Context(), "prj_first", attempts)
				if err != nil {
					return err
				}
				if len(events) != 17 {
					t.Fatalf("typed latest event population=%d", len(events))
				}
				for _, event := range events {
					var number int
					if _, err := fmt.Sscanf(event.AttemptID, "attempt_%d", &number); err != nil {
						return err
					}
					if event.OrganizationID != org || event.ProjectID != "prj_first" || event.Sequence != int64(1683+number) || event.RunID != "run_target" || event.ActorPrincipalID != "example-runner" || event.LandingHeadSHA != "head_target" {
						t.Fatalf("typed batch event lost source facts: %+v", event)
					}
				}
				if _, err := owner.LatestRunEvents(t.Context(), "prj_first", []string{attempts[0], attempts[0]}); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("duplicate batch identity=%v", err)
				}
				hidden, err := owner.LatestRunEvents(t.Context(), "prj_second", attempts)
				if err != nil {
					return err
				}
				if len(hidden) != 0 {
					t.Fatalf("typed batch crossed project: %+v", hidden)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
				t.Fatal(err)
			}
			probe := &pgstoretest.SearchPlanProbe{DBTX: tx}
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
				if row.RunID != "run_target" || !row.ActorEnvelopeKnown || row.ActorKind != "runner" || row.ActorPrincipalID != "example-runner" || row.ActorViaKey != "example-key" || row.ActorDisplayName != "Example actor" || row.ActorAgent != "example-agent" || !row.LandingWaiting || row.LandingChangeID != "change_target" || row.LandingVersionID != "version_target" || row.LandingHeadSHA != "head_target" {
					t.Fatalf("bounded diagnostics event lost full identity projection: %+v", row)
				}
				seen[row.AttemptID] = true
			}
			var raw []byte
			if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.Statement, probe.Args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plan []struct{ Plan pgstoretest.SearchPlanNode }
			if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
				t.Fatalf("decode plan: %v", err)
			}
			var visits func(pgstoretest.SearchPlanNode) float64
			visits = func(node pgstoretest.SearchPlanNode) float64 {
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
			config := pool.Config()
			config.MaxConns = 3
			capture := &diagnosticsQueryCapture{QueryTracer: config.ConnConfig.Tracer}
			config.ConnConfig.Tracer = capture
			observedPool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := postgres.NewRepository(t.Context(), observedPool, pgstoretest.SmallPool())
			if err != nil {
				observedPool.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := observed.Close(); err != nil {
					t.Error(err)
				}
			})
			var snapshot diagnosticstore.Snapshot
			if err := observed.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				snapshot, err = reader.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{"prj_first"}, pgstoretest.HistoryAt().Add(-time.Hour), pgstoretest.HistoryAt().Add(2*time.Hour), pgstoretest.HistoryAt().Add(2*time.Hour), "")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Landings) != 1 || snapshot.Landings[0].EnteredAt == nil || !snapshot.Landings[0].EnteredAt.Equal(pgstoretest.HistoryAt().Add(-2*time.Hour)) || !snapshot.Landings[0].At.Equal(pgstoretest.HistoryAt().Add(time.Hour)) {
				t.Fatalf("diagnostics lost exact pre-window merge entry or landing receipt: %+v", snapshot.Landings)
			}
			capture.mu.Lock()
			statement, args := capture.landing, append([]any(nil), capture.args...)
			capture.mu.Unlock()
			if statement == "" {
				t.Fatal("diagnostics did not execute its landing owner query")
			}
			var landingPlan []byte
			if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, args...).Scan(&landingPlan); err != nil {
				t.Fatal(err)
			}
			var landingNodes []struct{ Plan pgstoretest.SearchPlanNode }
			if err := json.Unmarshal(landingPlan, &landingNodes); err != nil || len(landingNodes) != 1 {
				t.Fatalf("decode diagnostic landing plan: %v", err)
			}
			indexed := false
			var inspect func(pgstoretest.SearchPlanNode)
			inspect = func(node pgstoretest.SearchPlanNode) {
				if node.Index == "collaboration_events_diagnostics_merge_entry_idx" {
					indexed = true
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			inspect(landingNodes[0].Plan)
			if !indexed || visits(landingNodes[0].Plan) > 4 {
				t.Fatalf("diagnostics merge entry walks event history: %s", landingPlan)
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
	admin := pgstoretest.Template.Database(t)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, "org_first", pgstoretest.HistoryAt()); err != nil {
		t.Fatal(err)
	}
	r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pgstoretest.SeedRepository(t, r, "org_first")
	f := pgstoretest.SeedHistory(t, r, "org_first", "first")
	for _, tc := range []struct {
		name   string
		replay func(eventstore.Writer, bool) error
	}{
		{"event", func(w eventstore.Writer, changed bool) error {
			record := f.Event
			record.Sequence = 0
			if changed {
				record.HeadSHA = "other"
			}
			got, err := w.AppendEvent(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.Event) {
				t.Error("event receipt changed")
			}
			return err
		}},
		{"snapshot", func(w eventstore.Writer, changed bool) error {
			record := f.Snapshot
			if changed {
				record.WorkItemID = "wi_first_2"
			}
			_, err := w.CreateCollaborationVersion(t.Context(), record)
			return err
		}},
		{"change", func(w eventstore.Writer, changed bool) error {
			record := f.Change
			if changed {
				record.Title = "other"
			}
			_, err := w.CreateChange(t.Context(), record)
			return err
		}},
		{"version", func(w eventstore.Writer, changed bool) error {
			record := f.Version
			if changed {
				record.HeadSHA = "other"
			}
			_, err := w.CreateVersion(t.Context(), record)
			return err
		}},
		{"evidence", func(w eventstore.Writer, changed bool) error {
			record := f.Evidence
			record.Sequence = 0
			if changed {
				record.Decision = "other"
			}
			got, err := w.AppendEvidence(t.Context(), record)
			if err == nil && !reflect.DeepEqual(got, f.Evidence) {
				t.Error("evidence receipt changed")
			}
			return err
		}},
		{"link", func(w eventstore.Writer, changed bool) error {
			record := f.Link
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
	next := pgstoretest.HistoryEvent(t, f.Content, "org_first", "event_next")
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
	admin := pgstoretest.Template.Database(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, organization, pgstoretest.HistoryAt()); err != nil {
			t.Fatal(err)
		}
	}
	r, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, organization)
	}
	if err := r.WriteLeases(t.Context(), "org_first", corestore.Serving, func(w leasestore.Writer) error {
		return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_first", ID: "machine_1", Hostname: "example", Capacity: 1, Version: "example"})
	}); err != nil {
		t.Fatal(err)
	}
	f := pgstoretest.SeedHistory(t, r, "org_first", "first")
	lease := pgstoretest.ClaimLease(t, r, "org_first", pgstoretest.LeaseClaim(1))
	attempt := pgstoretest.AttemptEvent(lease)
	attempt.ID = f.Version.AttemptID
	storedAttempt := pgstoretest.RecordLeaseAttempt(t, r, "org_first", attempt)
	value := tracker.NativeLandingReceipt{VersionID: f.Version.ID, MergeSHA: strings.Repeat("a", 40), BaseRef: "main", Landed: true}
	record, err := f.Content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: "org_first", AttemptID: storedAttempt.ID, VersionID: f.Version.ID, Repository: f.Version.Repository, BaseRef: value.BaseRef}, value)
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
	runtime := pgstoretest.RuntimeFor(t, pool, pool)
	for _, statement := range []string{"UPDATE change_landing_receipts SET merge_sha=merge_sha", "DELETE FROM change_landing_receipts"} {
		tx := pgstoretest.TenantTx(t, runtime, pool, "org_first")
		if _, err := tx.Exec(t.Context(), statement); err == nil {
			t.Fatalf("immutable landing receipt accepted %q", statement)
		} else {
			pgstoretest.RequireSQLState(t, err, "23514")
		}
		if err := tx.Rollback(t.Context()); err != nil {
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
	for _, test := range []struct{ name, attempt string }{
		{name: "unknown attempt", attempt: "attempt_unrelated"},
		{name: "missing attempt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrongAttempt := record
			wrongAttempt.AttemptID = test.attempt
			if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
				_, err := w.CreateLandingReceipt(t.Context(), wrongAttempt)
				return err
			}); !errors.Is(err, eventstore.ErrInvalidRecord) {
				t.Fatalf("unrelated attempt receipt error = %v, want invalid record", err)
			}
		})
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
		latest, err = reader.LatestLandingReceipt(t.Context(), f.Change.ProjectID, f.Change.WorkItemID)
		return err
	}); err != nil || latest != record {
		t.Fatalf("latest landing receipt = %+v, %v; want %+v", latest, err, record)
	}
	if err := r.ReadRecords(t.Context(), "org_second", corestore.Serving, func(reader eventstore.Reader) error {
		_, err := reader.LatestLandingReceipt(t.Context(), f.Change.ProjectID, f.Change.WorkItemID)
		return err
	}); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("cross-tenant latest landing receipt error = %v, want not found", err)
	}
	hydrated, err := f.Content.LandingReceipt(t.Context(), got)
	if err != nil || !reflect.DeepEqual(hydrated, value) {
		t.Fatalf("landing payload = %+v, %v; want %+v", hydrated, err, value)
	}
}

func TestLatestChangeUsesLinkCreationChronology(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, organization)
		content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
		olderAt := pgstoretest.HistoryAt()
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
			record, err := content.PrepareChange(t.Context(), organization, pgstoretest.HistoryJSON(t, value))
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
		t.Run(string(organization)+"/retained PR versions", func(t *testing.T) {
			makeVersion := func(change string, number int64, external string) eventstore.Version {
				t.Helper()
				value := tracker.ChangeVersion{ID: fmt.Sprintf("version_%s_%d", change, number), ChangeID: change, Number: number, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: fmt.Sprintf("head_%s_%d", change, number), BaseSHA: "base", MergeBaseSHA: "merge", Repository: "https://github.com/example/repo", PolicyID: "policy"}, Actor: pgstoretest.HistoryActor(), CreatedAt: newerAt.Add(time.Duration(number) * time.Second)}
				if external != "" {
					value.External = &tracker.ChangeExternalReference{Provider: "github", ID: external, URL: "https://github.com/example/repo/pull/" + external}
				}
				record, err := content.PrepareVersion(t.Context(), organization, pgstoretest.HistoryJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				return record
			}
			read := func(item tracker.NativeWorkItemID, want eventstore.Version) {
				t.Helper()
				if err := r.ReadRecords(t.Context(), organization, corestore.Serving, func(reader eventstore.Reader) error {
					retained, err := reader.WorkItemPullRequestVersion(t.Context(), "prj_first", item)
					if err != nil {
						return err
					}
					if retained.OrganizationID != organization || retained.ProjectID != "prj_first" || retained.WorkItemID != item || retained.ChangeID != want.ChangeID || retained.VersionID != want.ID || retained.Number != want.Number || retained.HeadSHA != want.HeadSHA || retained.Repository != want.Repository || retained.PolicyID != want.PolicyRevisionID || retained.AttemptID != want.AttemptID || retained.External != (tracker.ChangeExternalReference{Provider: want.ExternalProvider, ID: want.ExternalID, URL: want.ExternalURL}) {
						t.Fatalf("retained PR source=%+v, want immutable version %+v", retained, want)
					}
					canonical, err := reader.LatestChange(t.Context(), "prj_first", item)
					if err != nil {
						return err
					}
					if item == "wi_first_1" && canonical.ID != "a_newest" {
						t.Fatalf("retained PR changed canonical Change: %+v", canonical)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			write := func(rows ...eventstore.Version) {
				t.Helper()
				if err := r.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
					for _, row := range rows {
						if _, err := w.CreateVersion(t.Context(), row); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			older := makeVersion("z_oldest", 303, "303")
			write(older, makeVersion("z_oldest", 1, "1"), makeVersion("z_oldest", 304, ""), makeVersion("a_newest", 1, ""), older)
			read("wi_first_1", older)
			if err := r.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
				return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: "wi_first_2", ChangeID: "z_oldest"})
			}); err != nil {
				t.Fatal(err)
			}
			read("wi_first_2", older)
			newer := makeVersion("a_newest", 2, "21")
			write(newer)
			laterOlder := makeVersion("z_oldest", 305, "13")
			write(laterOlder, makeVersion("a_newest", 3, ""))
			read("wi_first_1", newer)
			read("wi_first_2", laterOlder)
			refused := makeVersion("z_oldest", 306, "99")
			errRollback := errors.New("Example rollback")
			if err := r.WriteRecords(t.Context(), organization, corestore.Serving, func(w eventstore.Writer) error {
				if _, err := w.CreateVersion(t.Context(), refused); err != nil {
					return err
				}
				return errRollback
			}); !errors.Is(err, errRollback) {
				t.Fatalf("rollback=%v", err)
			}
			read("wi_first_1", newer)
			read("wi_first_2", laterOlder)
			if err := r.ReadRecords(t.Context(), organization, corestore.Serving, func(reader eventstore.Reader) error {
				_, err := reader.Version(t.Context(), refused.ChangeID, refused.ID)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("rolled back version=%v", err)
				}
				_, err = reader.WorkItemPullRequestVersion(t.Context(), "prj_second", "wi_first_1")
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("foreign project retained PR=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventChangeImmutableHistory(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	r, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pgstoretest.SeedRepository(t, r, "org_first")
	pgstoretest.SeedHistory(t, r, "org_first", "first")
	runtime := pgstoretest.RuntimeFor(t, pool, pool)
	for _, table := range []string{"collaboration_events", "collaboration_versions", "change_versions", "change_evidence", "native_commands"} {
		for _, op := range []string{"update", "delete"} {
			t.Run(table+"/"+op, func(t *testing.T) {
				tx := pgstoretest.TenantTx(t, runtime, pool, "org_first")
				statement := "DELETE FROM " + table
				if op == "update" {
					statement = "UPDATE " + table + " SET payload_object_key=payload_object_key"
				}
				_, err := tx.Exec(t.Context(), statement)
				pgstoretest.RequireSQLState(t, err, "23514")
				if err := tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestEventChangeSequenceContention(t *testing.T) {
	for _, kind := range []string{"event", "event replay", "evidence", "evidence replay"} {
		t.Run(kind, func(t *testing.T) {
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			pgstoretest.SeedRepository(t, r, "org_first")
			f := pgstoretest.SeedHistory(t, r, "org_first", "first")
			preparedEvents := []eventstore.Event{pgstoretest.HistoryEvent(t, f.Content, "org_first", "contended1"), pgstoretest.HistoryEvent(t, f.Content, "org_first", "contended2")}
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
					evidence := f.Evidence
					evidence.Sequence = 0
					evidence.SourceKey = new("contended")
					if kind == "evidence" {
						evidence.SourceKey = new(fmt.Sprint(index))
					}
					got, err := w.AppendEvidence(ctx, evidence)
					return got.Sequence, err
				}
			}
			first := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
			pgstoretest.WaitRepositorySignal(t, ready)
			second := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
			if err := first.Wait(t); err != nil {
				t.Fatal(err)
			}
			if err := second.Wait(t); err != nil {
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
					records, err := reader.Evidence(t.Context(), f.Change.ID, 1, 10)
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
	admin := pgstoretest.Template.Database(t)
	r, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
	pgstoretest.SeedAccessTarget(t, admin, r, "org_first")
	f := pgstoretest.SeedHistory(t, r, "org_first", "project_cursor")
	firstEvent := historyEventForItem(t, f.Content, "org_first", "project_cursor_first", "wi_first_1")
	secondEvent := historyEventForItem(t, f.Content, "org_first", "project_cursor_second", "wi_first_2")
	firstSequence, secondSequence := int64(0), int64(0)
	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	first := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
	pgstoretest.WaitRepositorySignal(t, ready)
	second := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
	if err := first.Wait(t); err != nil {
		t.Fatal(err)
	}
	if err := second.Wait(t); err != nil {
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
		changes, err = reader.ProjectChanges(t.Context(), "prj_first", 1, highwater, pgstoretest.HistoryAt().Add(time.Hour), 201)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if highwater != 3 || len(changes) != 2 || changes[0].WorkItemID != "wi_first_1" || changes[1].WorkItemID != "wi_first_2" {
		t.Fatalf("project change page highwater=%d changes=%+v", highwater, changes)
	}
	pgstoretest.SeedAccessTarget(t, admin, r, "org_second")
	pgstoretest.SeedHistory(t, r, "org_second", "project_cursor_other")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := r.ReadRecords(t.Context(), org, corestore.Serving, func(reader eventstore.Reader) error {
			owner := reader.(eventstore.ProjectEventPageReader)
			rows, err := owner.ProjectEvents(t.Context(), "prj_first", 1, 1)
			if err != nil {
				return err
			}
			if org == "org_second" {
				if len(rows) != 0 {
					t.Fatalf("project event page crossed tenant: %+v", rows)
				}
				return nil
			}
			if len(rows) != 1 || rows[0].ID != firstEvent.ID || rows[0].ProjectSequence != 2 || rows[0].PayloadObjectKey != firstEvent.PayloadObjectKey {
				t.Fatalf("raw project event first page = %+v", rows)
			}
			rows, err = owner.ProjectEvents(t.Context(), "prj_first", 2, 1)
			if err == nil && (len(rows) != 1 || rows[0].ID != secondEvent.ID || rows[0].ProjectSequence != 3) {
				t.Fatalf("raw project event continuation = %+v", rows)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
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
			r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
			pgstoretest.SeedRepository(t, r, "org_first")
			f := pgstoretest.SeedHistory(t, r, "org_first", "first")
			for index, sequence := range []int64{40, 12, 0} {
				record := pgstoretest.HistoryEvent(t, f.Content, "org_first", fmt.Sprintf("import_%d", index))
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
						record := f.Evidence
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
					rows, err := reader.Evidence(t.Context(), f.Change.ID, 1, 10)
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
			admin := pgstoretest.Template.Database(t)
			r, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 3, BackgroundConnections: 1})
			content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
			first, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_first", ActorID: "example", Operation: "create_change", CommandKey: "concurrent", RequestHash: artifact.Digest([]byte("first"))}, []byte(`{"result":"original"}`))
			if err != nil {
				t.Fatal(err)
			}
			next, err := content.PrepareReceipt(t.Context(), first, []byte(`{"result":"original"}`))
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
			holder := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
			pgstoretest.WaitRepositorySignal(t, ready)
			waiter := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
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
			if err := holder.Wait(t); err != nil {
				t.Fatal(err)
			}
			err = waiter.Wait(t)
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if _, err := admin.ExecContext(t.Context(), `ALTER TABLE change_evidence SET (autovacuum_enabled=false); ALTER TABLE current_change_reviews SET (autovacuum_enabled=false)`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if org == "org_second" {
			if _, err := admin.ExecContext(t.Context(), `ANALYZE change_evidence; ANALYZE current_change_reviews`); err != nil {
				t.Fatal(err)
			}
		}
		pgstoretest.SeedRepository(t, repository, org)
		for _, actors := range []int{17, 10000} {
			key := fmt.Sprintf("current_%d", actors)
			fixture := pgstoretest.SeedHistory(t, repository, org, key)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence
    (quality_at,project_id,organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_session_known)
    SELECT $4::timestamptz,(SELECT project_id FROM change_requests WHERE organization_id=$1 AND id=$2),$1,$2,n+1,$3,'review','human','actor_'||lpad(((n-1)%$7)::text,5,'0'),
    CASE WHEN n%37=0 THEN 'commented' WHEN ((n-1)%$7)%2=0 THEN 'changes_requested' ELSE 'approved' END,
    $4::timestamptz,$5,$6,1,1,'human',true FROM generate_series(1,30000) n`, org, fixture.Change.ID, fixture.Version.ID, pgstoretest.HistoryAt(), fixture.Evidence.PayloadObjectKey, fixture.Evidence.PayloadSHA256, actors); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO current_change_reviews(organization_id,change_id,version_id,review_kind,actor_key,decision,evidence_sequence)
    SELECT DISTINCT ON (actor_principal_id) organization_id,change_id,version_id,'human',actor_principal_id,decision,sequence FROM change_evidence
    WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND decision<>'commented'
    ORDER BY actor_principal_id,sequence DESC`, org, fixture.Change.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET evidence_sequence=30001,review_summary_sequence=30001,current_version_id=$3,review_evidence_revision=review_evidence_revision+1 WHERE organization_id=$1 AND id=$2`, org, fixture.Change.ID, fixture.Version.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
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
						tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback(t.Context())
						if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
							t.Fatal(err)
						}
						probe := &pgstoretest.SearchPlanProbe{DBTX: tx}
						queries := pgquery.New(probe)
						args := pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: fmt.Sprintf("change_current_%d", actors), VersionID: fmt.Sprintf("version_current_%d", actors), ReviewingActor: actor, Through: 30001}
						sequences, err := queries.RecordCurrentChangeReviewSequences(t.Context(), args)
						if err != nil {
							t.Fatal(err)
						}
						headSQL, headArgs := probe.Statement, append([]any(nil), probe.Args...)
						rows, err := queries.RecordCurrentChangeEvidenceBySequences(t.Context(), pgquery.RecordCurrentChangeEvidenceBySequencesParams{OrganizationID: string(org), ChangeID: args.ChangeID, Sequences: sequences})
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
						}{{headSQL, headArgs}, {probe.Statement, append([]any(nil), probe.Args...)}} {
							var raw []byte
							if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement.sql, statement.args...).Scan(&raw); err != nil {
								t.Fatal(err)
							}
							var plans []struct{ Plan pgstoretest.SearchPlanNode }
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
	t.Run("recovery sparse evidence seeks", func(t *testing.T) {
		ctx := t.Context()
		const org tracker.OrganizationID = "org_first"
		fixture := pgstoretest.SeedHistory(t, repository, org, "recovery_seek")
		current, err := fixture.Content.Version(ctx, fixture.Version)
		if err != nil {
			t.Fatal(err)
		}
		current.ID, current.Number = "version_recovery_seek_current", 2
		currentRow, err := fixture.Content.PrepareVersion(ctx, org, pgstoretest.HistoryJSON(t, current))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRecords(ctx, org, corestore.Serving, func(w eventstore.Writer) error {
			_, err := w.CreateVersion(ctx, currentRow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `
WITH seeded(sequence,version_id,kind,review_kind,review_landing,decision) AS (
 SELECT 2*n,$3::text,'discussion','',false,'' FROM generate_series(1,32) n
 UNION ALL SELECT 2*n+1,NULL::text,'discussion','',false,'' FROM generate_series(1,32) n
 UNION ALL SELECT 66,$3,'review','security_audit',true,'approved'
 UNION ALL SELECT 67,$4::text,'review','',false,'approved'
 UNION ALL SELECT 67+n,$3,'review','',false,'approved' FROM generate_series(1,200) n
 UNION ALL SELECT 1000+n,$3,'review','security_audit',false,'commented' FROM generate_series(1,1000) n
 UNION ALL SELECT 2000+n,$4,'review','',false,'changes_requested' FROM generate_series(1,1000) n
 UNION ALL SELECT 3000+n,$4,'discussion','',false,'' FROM generate_series(1,1000) n
 UNION ALL SELECT 4000+n,$3,'check','',false,'' FROM generate_series(1,10000) n
)
INSERT INTO change_evidence (quality_at,project_id,organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,
 payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_landing)
SELECT CASE WHEN kind='review' THEN $5::timestamptz END,(SELECT project_id FROM change_requests WHERE organization_id=$1 AND id=$2),$1,$2,sequence,version_id,kind,'human','example',decision,$5,$6,$7,1,1,review_kind,review_landing FROM seeded`,
			org, fixture.Change.ID, currentRow.ID, fixture.Version.ID, pgstoretest.HistoryAt(), fixture.Evidence.PayloadObjectKey, fixture.Evidence.PayloadSHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, "ANALYZE change_evidence"); err != nil {
			t.Fatal(err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SELECT set_config('detent.organization_id', $1, true)", string(org)); err != nil {
			t.Fatal(err)
		}
		checkVisits := func(statement string, args []any, subplan string, bound float64) {
			t.Helper()
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan json.RawMessage }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode recovery lookup plan: %v", err)
			}
			plan := plans[0].Plan
			if subplan != "" {
				var root struct {
					Plans []json.RawMessage
				}
				if err := json.Unmarshal(plan, &root); err != nil {
					t.Fatal(err)
				}
				plan = nil
				for _, child := range root.Plans {
					var node struct {
						Name string `json:"Subplan Name"`
					}
					if err := json.Unmarshal(child, &node); err != nil {
						t.Fatal(err)
					}
					if node.Name == subplan {
						plan = child
						break
					}
				}
				if plan == nil {
					t.Fatalf("missing bounded subplan %q: %s", subplan, raw)
				}
			}
			var node pgstoretest.SearchPlanNode
			if err := json.Unmarshal(plan, &node); err != nil {
				t.Fatal(err)
			}
			visited := currentReviewLookupVisits(node)
			if visited == 0 || visited > bound {
				t.Fatalf("recovery evidence visited %.0f rows, want at most %.0f: %s", visited, bound, raw)
			}
		}
		const through = 14000
		priorProbe := &pgstoretest.SearchPublicationProbe{Tx: tx}
		prior, err := pgquery.New(priorProbe).RecordChangePriorApproval(ctx, pgquery.RecordChangePriorApprovalParams{
			OrganizationID: org, ChangeID: fixture.Change.ID, VersionID: new(currentRow.ID), Through: through,
		})
		if err != nil || prior.Approved || !prior.Unprojected {
			t.Fatalf("prior approval classification: %+v, %v", prior, err)
		}
		checkVisits(priorProbe.Statement, priorProbe.Args, "CTE unknown_approvals", 201)

		probe := &pgstoretest.SearchPlanProbe{DBTX: tx}
		rows, err := pgquery.New(probe).RecordRecoveryChangeEvidence(ctx, pgquery.RecordRecoveryChangeEvidenceParams{
			OrganizationID: org, ChangeID: fixture.Change.ID, VersionID: new(currentRow.ID), Through: through, IncludePriorUnprojected: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var discussions []int64
		var landing, unknownPrior int64
		for _, row := range rows {
			switch {
			case row.Kind == "discussion":
				discussions = append(discussions, row.Sequence)
			case row.ReviewLanding:
				landing = row.Sequence
			case row.ReviewKind == "" && row.VersionID != nil && *row.VersionID == fixture.Version.ID:
				unknownPrior = row.Sequence
			}
		}
		var want []int64
		for i := range 32 {
			want = append(want, int64(i+34))
		}
		if !slices.Equal(discussions, want) || landing != 66 || unknownPrior != 67 {
			t.Fatalf("recovery seek lost older matching evidence: discussions=%v landing=%d prior=%d", discussions, landing, unknownPrior)
		}
		checkVisits(probe.Statement, probe.Args, "", 32+1+64+201)
	})
}

func currentReviewLookupVisits(node pgstoretest.SearchPlanNode) float64 {
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		f := pgstoretest.SeedHistory(t, repository, org, "machine_plan")
		for index, kind := range []string{"validator", "implementation", "security_audit"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence
(quality_at,project_id,organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_landing,review_session_known)
SELECT $6::timestamptz,(SELECT project_id FROM change_requests WHERE organization_id=$1 AND id=$2),$1,$2,$4+n+1,$3,'review','worker',$5||'_'||n::text,CASE WHEN n%2=0 THEN 'commented' ELSE 'approved' END,
$6::timestamptz,$7,$8,1,1,$5,($5='security_audit' AND n>20000),true FROM generate_series(1,30000)n`, org, f.Change.ID, f.Version.ID, index*30000, kind, pgstoretest.HistoryAt(), f.Evidence.PayloadObjectKey, f.Evidence.PayloadSHA256); err != nil {
				t.Fatal(err)
			}
		}
	}
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
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
				{through: 0}, {through: 1}, {through: 4, want: []int64{4}}, {through: 30001, want: []int64{30001}},
				{through: 60001, want: []int64{30001, 60001}}, {through: 90001, want: []int64{30001, 60001, 80001}}, {through: 90001, missing: true},
			} {
				t.Run(fmt.Sprintf("%s/analyzed=%t/through=%d/missing=%t", org, analyzed, test.through, test.missing), func(t *testing.T) {
					tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
						t.Fatal(err)
					}
					probe := &pgstoretest.SearchPlanProbe{DBTX: tx}
					change := "change_machine_plan"
					if test.missing {
						change = "change_missing"
					}
					got, err := pgquery.New(probe).RecordCurrentChangeReviewSequences(t.Context(), pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: change, VersionID: "version_machine_plan", ReviewingActor: "example-owner", Through: test.through})
					if err != nil || !slices.Equal(got, test.want) {
						t.Fatalf("machine prefix=%v want=%v error=%v", got, test.want, err)
					}
					var raw []byte
					if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.Statement, probe.Args...).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plans []struct{ Plan pgstoretest.SearchPlanNode }
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	pool := pgstoretest.RolePool(t, admin, "detent_runtime_v2", "")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		for _, kind := range []string{"validator", "implementation", "security_audit"} {
			t.Run(string(org)+"/"+kind, func(t *testing.T) {
				f := pgstoretest.SeedHistory(t, repository, org, kind)
				if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET current_version_id=$3 WHERE organization_id=$1 AND id=$2`, org, f.Change.ID, f.Version.ID); err != nil {
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
					review := tracker.ChangeReview{ID: fmt.Sprintf("review_%s_%d", kind, reviewNumber), VersionID: f.Version.ID, Decision: decision, Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt()}
					result := &gate.ValidatorResult{VersionID: f.Version.ID, SessionID: 1, Verdict: verdict}
					switch kind {
					case "validator":
						review.Validator = result
					case "implementation":
						review.ImplementationReview = result
					case "security_audit":
						review.SecurityAudit = &tracker.SecurityAuditReview{VersionID: f.Version.ID, SessionID: 1, Landing: landing}
					}
					row, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "review"}, pgstoretest.HistoryJSON(t, review))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
						t.Fatal(err)
					}
					if _, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil {
						t.Fatal(err)
					}
				}
				assertHead := func(sequence int64, decision string) {
					t.Helper()
					tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
						t.Fatal(err)
					}
					sequences, err := pgquery.New(tx).RecordCurrentChangeReviewSequences(t.Context(), pgquery.RecordCurrentChangeReviewSequencesParams{OrganizationID: org, ChangeID: f.Change.ID, VersionID: f.Version.ID, ReviewingActor: "example-owner", Through: reviewNumber + 1})
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
						evidence, _, err = r.(eventstore.ChangeReviewEvidenceReader).ChangeReviewEvidence(t.Context(), f.Change.ID, f.Version.ID, sequence, nil, "example-owner")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					version := tracker.ChangeVersion{ID: f.Version.ID, ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: "example-policy"}, Policy: policy.Descriptor{ID: "example-policy"}, ReviewPolicy: tracker.ChangeReviewPolicy{ID: "example-review-policy"}}
					if kind == "validator" {
						version.Policy.Gates.Validator = true
					} else {
						version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
					}
					detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: f.Version.ID}, Versions: []tracker.ChangeVersion{version}}
					for _, row := range evidence {
						raw, err := f.Content.Evidence(t.Context(), row)
						if err != nil {
							t.Fatal(err)
						}
						review, err := eventstore.DecodeChangeReview(row, raw)
						if err != nil {
							t.Fatal(err)
						}
						detail.Reviews = append(detail.Reviews, review)
					}
					summary := changerequest.SummarizeCurrentVersion(detail, version.PolicyID, version.ReviewPolicy.ID, pgstoretest.HistoryAt(), false)
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
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	fixtures := map[tracker.OrganizationID]pgstoretest.HistoryFixture{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		f := pgstoretest.SeedHistory(t, repository, org, "backfill")
		change, err := f.Content.Change(t.Context(), f.Change)
		if err != nil {
			t.Fatal(err)
		}
		change.CurrentVersion, change.Revision = f.Version.ID, 2
		updated, err := f.Content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, change))
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
			review := tracker.ChangeReview{ID: fmt.Sprintf("review_%d", i), VersionID: f.Version.ID, Decision: decision, Actor: tracker.Actor{Kind: "human", PrincipalID: fmt.Sprintf("reviewer_%d", i%2)}, CreatedAt: pgstoretest.HistoryAt()}
			row, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "review"}, pgstoretest.HistoryJSON(t, review))
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
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM current_change_reviews WHERE organization_id=$1 AND change_id=$2`, org, f.Change.ID).Scan(&derived); err != nil || derived != 0 {
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
					rows, _, err = r.(eventstore.ChangeReviewEvidenceReader).ChangeReviewEvidence(t.Context(), f.Change.ID, f.Version.ID, through, nil, "new_reviewer")
					return err
				})
				return rows, err
			}
			if _, err := read(302); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unbuilt current reviews accepted: %v", err)
			}
			prepare := func(id, actor, decision string, sequence int64, known bool) eventstore.Evidence {
				t.Helper()
				row, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "review", Sequence: sequence, SourceKey: new(id)}, pgstoretest.HistoryJSON(t, tracker.ChangeReview{ID: id, VersionID: f.Version.ID, Decision: decision, Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: pgstoretest.HistoryAt()}))
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
			probe := currentReviewReadProbe{DetailStore: f.Objects, before: func(ctx context.Context) error {
				tx, err := admin.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				var id string
				err = tx.QueryRowContext(ctx, `SELECT id FROM change_requests WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, org, f.Change.ID).Scan(&id)
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
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 103 {
				t.Fatalf("final current review page=%d error=%v", n, err)
			}
			rows, err := read(303)
			if err != nil || len(rows) != 2 || rows[0].Sequence != 302 || rows[1].Sequence != 303 {
				t.Fatalf("forward review lost during backfill: rows=%+v error=%v", rows, err)
			}
			var before, after eventstore.Change
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				before, err = r.Change(t.Context(), f.Change.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := appendRow(t.Context(), future); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				after, err = r.Change(t.Context(), f.Change.ID)
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
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 1 {
				t.Fatalf("sparse source page=%d error=%v", n, err)
			}
			late := prepare("review_late", "reviewer_0", "approved", 400, true)
			if err := appendRow(t.Context(), late); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
				var err error
				after, err = r.Change(t.Context(), f.Change.ID)
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
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 2 {
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
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 1 {
				t.Fatalf("legacy source page=%d error=%v", n, err)
			}
			if _, err := read(500); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unfinished suffix accepted: %v", err)
			}
			if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 1 {
				t.Fatalf("legacy suffix page=%d error=%v", n, err)
			}
			rows, err = read(500)
			if err != nil || len(rows) != 2 || rows[0].Sequence != 450 || rows[1].Sequence != 500 {
				t.Fatalf("legacy review not recovered: rows=%+v error=%v", rows, err)
			}
			discussion, err := f.Content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: f.Change.ID, Kind: "discussion"}, pgstoretest.HistoryJSON(t, tracker.ChangeDiscussion{ID: "current_discussion", VersionID: f.Version.ID, Body: "Current feedback", Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt()}))
			if err != nil {
				t.Fatal(err)
			}
			if err := appendRow(t.Context(), discussion); err != nil {
				t.Fatal(err)
			}
			neutralRows, err := read(501)
			if err != nil || !reflect.DeepEqual(rows, neutralRows) {
				t.Fatalf("discussion invalidated current review evidence: rows=%+v error=%v", neutralRows, err)
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
					probe := currentReviewReadProbe{DetailStore: f.Objects, before: func(ctx context.Context) error {
						if interleaved {
							return nil
						}
						interleaved = true
						if test.late != 0 {
							return appendRow(ctx, prepare(fmt.Sprintf("late_%d", test.late), "reviewer_1", "changes_requested", test.late, test.known))
						}
						_, err := repository.BackfillCurrentChangeReviews(ctx, org, f.Content, 200)
						return err
					}}
					if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, eventstore.NewContent(probe), 200); n != 0 || !errors.Is(err, corestore.ErrRevisionConflict) || !interleaved {
						t.Fatalf("stale backfill committed: rows=%d interleaved=%t error=%v", n, interleaved, err)
					}
					if test.late != 0 {
						if _, err := read(test.tip); !errors.Is(err, corestore.ErrRevisionConflict) {
							t.Fatalf("late insertion left stale coverage certified: %v", err)
						}
						if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != map[bool]int{true: 2, false: 1}[test.known] {
							t.Fatalf("late insertion rebuild rows=%d error=%v", n, err)
						}
						if !test.known {
							if n, err := repository.BackfillCurrentChangeReviews(t.Context(), org, f.Content, 200); err != nil || n != 1 {
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
	admin := pgstoretest.Template.Database(t)
	admin.SetMaxOpenConns(1)
	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261104114549, false); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE INDEX change_current_machine_reviews_idx ON change_evidence(organization_id,change_id,version_id,review_kind,sequence DESC) WHERE kind='review'`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.ApplyVersion(t.Context(), 20261104114549, true)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "P0001" {
		t.Fatalf("wrong machine review predicate was accepted: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY change_current_machine_reviews_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261104114549, true); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261104114549, false); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := admin.QueryRowContext(t.Context(), `SELECT to_regclass('public.change_current_machine_reviews_idx') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("machine index rollback exists=%t error=%v", exists, err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261104114549, true); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentReviewMigrationPreservesSourceIdentityOnRetry(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	admin.SetMaxOpenConns(1)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		pgstoretest.SeedHistory(t, repository, org, "migration")
	}
	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE UNIQUE INDEX change_evidence_source_identity_restore_idx ON change_evidence(organization_id,change_id,kind,source_key) NULLS NOT DISTINCT`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.ApplyVersion(t.Context(), 20261104090048, false)
	var nullSemantics *pgconn.PgError
	if !errors.As(err, &nullSemantics) || nullSemantics.Code != "P0001" {
		t.Fatalf("NULLS NOT DISTINCT restore index was not refused: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY change_evidence_source_identity_restore_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261104090048, false); err != nil {
		t.Fatal(err)
	}
	check := func(phase int) {
		t.Helper()
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			_, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id,quality_at)
    SELECT organization_id,change_id,$3,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id,quality_at FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=1`, org, "change_migration", int64(1000+phase*10))
			var duplicate *pgconn.PgError
			if !errors.As(err, &duplicate) || duplicate.Code != "23505" {
				t.Fatalf("nonnull source identity phase=%d org=%s error=%v", phase, org, err)
			}
			for i := range 2 {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO change_evidence(organization_id,change_id,sequence,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id,quality_at)
     SELECT organization_id,change_id,$3,version_id,kind,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id,quality_at FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=1`, org, "change_migration", int64(1001+phase*10+i)); err != nil {
					t.Fatalf("null source identity phase=%d org=%s error=%v", phase, org, err)
				}
			}
		}
	}
	check(0)
	if _, err := admin.ExecContext(t.Context(), `CREATE UNIQUE INDEX change_evidence_source_identity_idx ON change_evidence(change_id,organization_id,kind,source_key) WHERE source_key IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	_, err = provider.ApplyVersion(t.Context(), 20261104090048, true)
	var refused *pgconn.PgError
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
	if _, err := provider.ApplyVersion(t.Context(), 20261104090048, true); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := provider.ApplyVersion(t.Context(), 20261104090048, false); err != nil {
		t.Fatal(err)
	}
	check(2)
	if _, err := provider.ApplyVersion(t.Context(), 20261104090048, true); err != nil {
		t.Fatal(err)
	}
	check(3)
}

type diagnosticsQueryCapture struct {
	pgx.QueryTracer
	mu              sync.Mutex
	landing         string
	args            []any
	scheduler       string
	schedulerArgs   []any
	predecessor     string
	predecessorArgs []any
}

func (p *diagnosticsQueryCapture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "WITH candidates AS MATERIALIZED (") && strings.Contains(data.SQL, "scheduler_outcome") {
		p.mu.Lock()
		p.scheduler, p.schedulerArgs = data.SQL, append([]any(nil), data.Args...)
		p.mu.Unlock()
	}
	if strings.HasPrefix(data.SQL, "SELECT requested.claim_id,prior.*") {
		p.mu.Lock()
		p.predecessor, p.predecessorArgs = data.SQL, append([]any(nil), data.Args...)
		p.mu.Unlock()
	}
	if strings.Contains(data.SQL, "FROM selected c LEFT JOIN LATERAL") && strings.Contains(data.SQL, "landed_at") {
		p.mu.Lock()
		p.landing, p.args = data.SQL, append([]any(nil), data.Args...)
		p.mu.Unlock()
	}
	if p.QueryTracer != nil {
		return p.QueryTracer.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}
func (p *diagnosticsQueryCapture) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if p.QueryTracer != nil {
		p.QueryTracer.TraceQueryEnd(ctx, conn, data)
	}
}

func (p *diagnosticsQueryCapture) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if tracer, ok := p.QueryTracer.(pgx.BatchTracer); ok {
		return tracer.TraceBatchStart(ctx, conn, data)
	}
	return ctx
}
func (p *diagnosticsQueryCapture) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if tracer, ok := p.QueryTracer.(pgx.BatchTracer); ok {
		tracer.TraceBatchQuery(ctx, conn, data)
	}
}
func (p *diagnosticsQueryCapture) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	if tracer, ok := p.QueryTracer.(pgx.BatchTracer); ok {
		tracer.TraceBatchEnd(ctx, conn, data)
	}
}

type analyticsQualityQueryCapture struct {
	mu   sync.Mutex
	sql  map[string]string
	args map[string][]any
}

func (c *analyticsQualityQueryCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	family := ""
	if strings.Contains(data.SQL, "SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.type='workflow.transitioned'") {
		family = "workflow"
	}
	if strings.Contains(data.SQL, "SELECT e.* FROM change_evidence e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.quality_at IS NOT NULL") {
		family = "evidence"
	}
	if family != "" {
		c.mu.Lock()
		if c.sql == nil {
			c.sql = map[string]string{}
			c.args = map[string][]any{}
		}
		c.sql[family] = data.SQL
		c.args[family] = append([]any(nil), data.Args...)
		c.mu.Unlock()
	}
	return ctx
}
func (*analyticsQualityQueryCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}
