package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedRetentionChange(t *testing.T, f *postgresStorageArchiveFixture, item tracker.NativeWorkItemID, landed, newVersion bool) tracker.ChangeRequest {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	change := tracker.ChangeRequest{ID: "chg_retention", OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item, Title: "Retention change", Revision: 1, CreatedAt: f.now(), UpdatedAt: f.now()}
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	row, err := content.PrepareChange(t.Context(), f.project.OrganizationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, func(w tenantstore.Writer) error { _, err := w.CreateChange(t.Context(), row); return err })
	count := 1
	if newVersion {
		count = 2
	}
	for number := 1; number <= count; number++ {
		id := "cv_retention"
		if number == 2 {
			id = "cv_retention_new"
		}
		version := tracker.ChangeVersion{ID: id, ChangeID: change.ID, Number: int64(number), ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: id + "_head", BaseSHA: "base", MergeBaseSHA: "merge-base", Repository: "owner/repository", PolicyID: "policy"}, Actor: tracker.Actor{Kind: "integration", PrincipalID: "retention_fixture"}, CreatedAt: f.now()}
		raw, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		record, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, raw)
		if err != nil {
			t.Fatal(err)
		}
		f.write(t, func(w tenantstore.Writer) error { _, err := w.CreateVersion(t.Context(), record); return err })
		change.CurrentVersion = id
	}
	if landed {
		change.Landed = &tracker.ChangeLanding{VersionID: "cv_retention", HeadSHA: "cv_retention_head", MergeSHA: "merged", BaseRef: "develop", LandedAt: f.now()}
	}
	change = updateRetentionChange(t, f, change)
	f.write(t, func(w tenantstore.Writer) error {
		return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item, ChangeID: change.ID, CreatedAt: f.now()})
	})
	return change
}

func updateRetentionChange(t *testing.T, f *postgresStorageArchiveFixture, change tracker.ChangeRequest) tracker.ChangeRequest {
	t.Helper()
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareChange(t.Context(), f.project.OrganizationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, func(w tenantstore.Writer) error {
		var err error
		row, err = w.UpdateChange(t.Context(), row)
		return err
	})
	change.Revision = tracker.Revision(row.Revision)
	return change
}

func landRetentionChange(t *testing.T, f *postgresStorageArchiveFixture, change tracker.ChangeRequest) {
	t.Helper()
	change.Landed = &tracker.ChangeLanding{VersionID: change.CurrentVersion, HeadSHA: change.CurrentVersion + "_head", MergeSHA: "merged", BaseRef: "develop", LandedAt: f.now()}
	updateRetentionChange(t, f, change)
}

func retentionSlotHistory(t *testing.T, f *postgresStorageArchiveFixture, item tracker.NativeWorkItemID) func() {
	t.Helper()
	now := f.now().Truncate(time.Hour)
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_history','example','example',3,'test',$2,$2,$2)`, f.project.OrganizationID, now); err != nil {
		t.Fatal(err)
	}
	ages := []time.Duration{31*24*time.Hour + time.Hour, 31 * 24 * time.Hour, 30*24*time.Hour + time.Hour, 30 * 24 * time.Hour, 30*24*time.Hour - time.Hour, 29 * 24 * time.Hour}
	expected := map[time.Time]bool{}
	for index, age := range ages {
		acquired := now.Add(-age)
		finished := acquired.Add(time.Hour)
		if index > 0 && index < len(ages)-1 {
			expected[acquired] = true
		}
		for slot := range 3 {
			id := fmt.Sprintf("historical_lease_%d_%d", index, slot)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT organization_id,$3,id,'machine_history',$3,revision,$4,$4,$5,$5 FROM issues WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, item, id, acquired, finished); err != nil {
				t.Fatal(err)
			}
		}
	}
	window := operatortool.AnalyticsWindow{From: now.Add(-31 * 24 * time.Hour), To: now.Add(-29 * 24 * time.Hour)}
	read := func() []diagnosticsCapacity {
		t.Helper()
		var snapshot diagnosticstore.Snapshot
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			snapshot, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{f.project.ID}, window.From, window.To, f.now(), "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if snapshot.Partial || len(snapshot.Leases) != 12 {
			t.Fatalf("historical lease window partial=%v leases=%d", snapshot.Partial, len(snapshot.Leases))
		}
		report := projectTenantDiagnostics(snapshot, window)
		found := 0
		for _, bucket := range report.Capacity {
			if expected[bucket.Hour] {
				found++
				if bucket.Slots != 3 {
					t.Fatalf("historical capacity at %s=%v want3", bucket.Hour, bucket.Slots)
				}
			} else if bucket.Slots != 0 {
				t.Fatalf("out-of-window slots at %s=%v", bucket.Hour, bucket.Slots)
			}
		}
		if found != len(expected) {
			t.Fatalf("historical capacity buckets=%d want%d", found, len(expected))
		}
		return report.Capacity
	}
	before := read()
	return func() {
		t.Helper()
		after := read()
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("retention changed historical slot output: before=%+v after=%+v", before, after)
		}
		var retained int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM leases WHERE organization_id=$1 AND machine_id='machine_history'`, f.project.OrganizationID).Scan(&retained); err != nil || retained != 18 {
			t.Fatalf("historical source leases=%d error=%v", retained, err)
		}
	}
}

type retentionInterleavingDetails struct {
	attachment.DetailStore
	triggered  atomic.Bool
	interleave func()
	readKey    string
	readError  error
}

func (d *retentionInterleavingDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := d.DetailStore.StoreDetail(ctx, organization, raw)
	if err == nil && d.interleave != nil && d.triggered.CompareAndSwap(false, true) {
		d.interleave()
	}
	return ref, err
}

func (d *retentionInterleavingDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if d.readError != nil && ref.Key == d.readKey {
		return nil, d.readError
	}
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestNativeIssueRetentionRechecksCurrentAuthorityBeforeCommit(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"source changed", "period disabled", "lease acquired", "cursor advanced"} {
		t.Run(change, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			issue := f.closed(t)
			f.clock.Add(int64(31 * 24 * time.Hour))
			details := &retentionInterleavingDetails{DetailStore: f.service.runDetailStore()}
			details.interleave = func() {
				f.checkIO(t)
				switch change {
				case "source changed":
					f.transition(t, issue.WorkItemID, "Todo")
				case "period disabled":
					f.write(t, func(w tenantstore.Writer) error {
						project, err := w.ProjectRecord(t.Context(), f.project.ID)
						if err != nil {
							return err
						}
						project.Settings.ArchiveCompletedAfterDays = nil
						_, err = w.UpdateProjectRecord(t.Context(), project)
						return err
					})
				case "cursor advanced":
					var cursor corestore.WorkItemRetentionCursor
					var page corestore.WorkItemRetentionPage
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
						owner := r.(corestore.WorkItemRetentionReader)
						var err error
						cursor, err = owner.WorkItemRetentionCursor(t.Context())
						if err != nil {
							return err
						}
						page, err = owner.WorkItemRetentionCandidates(t.Context(), f.now(), cursor, 4)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if len(page.Items) != 1 {
						t.Fatalf("cursor race candidates=%d", len(page.Items))
					}
					f.write(t, func(w tenantstore.Writer) error {
						changed, err := w.(corestore.WorkItemRetentionWriter).AdvanceWorkItemRetention(t.Context(), cursor, page.Items[0].Cursor)
						if err == nil && !changed {
							t.Fatal("cursor race did not advance the existing owner")
						}
						return err
					})
				case "lease acquired":
					admin := nativeAttemptTestDatabase(t, f.nativeFixture)
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_race','example','example',1,'test',$2,$2,$2)`, f.project.OrganizationID, f.now()); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT organization_id,'lease_race',id,'machine_race','session_race',revision,$3,$3,$4 FROM issues WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID, f.now(), f.now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.service.config.RunDetails = details
			err := f.service.retainClosedWorkItems(t.Context(), f.project.OrganizationID, f.now())
			if err != nil {
				t.Fatal(err)
			}
			if !details.triggered.Load() {
				t.Fatal("retention publication did not exercise current-owner race")
			}
			if got := readWorkItem(t, f.nativeFixture, issue.WorkItemID, ""); got.Archived != (change == "cursor advanced") {
				t.Fatal("retention did not honor current authority or benign concurrent cursor progress")
			}
		})
	}
	for _, fault := range []struct {
		name    string
		failure error
		item    bool
	}{{"missing", artifact.ErrMissing, true}, {"integrity", artifact.ErrIntegrity, true}, {"backend", artifact.ErrStorage, false}} {
		t.Run(fault.name, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			items := make([]tracker.NativeIssue, 0, 6)
			for range 6 {
				items = append(items, f.closed(t))
			}
			f.clock.Add(int64(31 * 24 * time.Hour))
			var version eventstore.CollaborationVersion
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				version, err = r.CollaborationVersion(t.Context(), string(items[0].WorkItemID), int64(items[0].Revision))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			details := &retentionInterleavingDetails{DetailStore: f.service.runDetailStore(), readKey: version.PayloadObjectKey, readError: fault.failure}
			f.service.config.RunDetails = details
			if err := f.service.retainClosedWorkItems(t.Context(), f.project.OrganizationID, f.now()); !errors.Is(err, fault.failure) {
				t.Fatalf("item failure=%v want%v", err, fault.failure)
			}
			count := func() int {
				t.Helper()
				var count int
				if err := nativeAttemptTestDatabase(t, f.nativeFixture).QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND archived`, f.project.OrganizationID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			if fault.item {
				if got := count(); got != 3 {
					t.Fatalf("item failure starved later candidates: archived=%d want3", got)
				}
				if err := f.service.retainClosedWorkItems(t.Context(), f.project.OrganizationID, f.now()); err != nil {
					t.Fatal(err)
				}
				if got := count(); got != 5 {
					t.Fatalf("resumed item failure starved tail: archived=%d want5", got)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					item, err := r.WorkItem(t.Context(), f.project.ID, items[0].WorkItemID)
					if err == nil && item.Archived {
						t.Fatal("invalid source item was archived")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if got := count(); got != 0 {
					t.Fatalf("backend failure advanced archive work: archived=%d", got)
				}
				details.readError = nil
				for range 2 {
					if err := f.service.retainClosedWorkItems(t.Context(), f.project.OrganizationID, f.now()); err != nil {
						t.Fatal(err)
					}
				}
				if got := count(); got != 6 {
					t.Fatalf("backend recovery missed candidates: archived=%d want6", got)
				}
			}
		})
	}
}

func TestTerminalIssueArchiveFollowsLostAttemptLease(t *testing.T) {
	t.Parallel()
	f := newPostgresStorageArchiveFixture(t)
	repository, ok := f.service.config.TenantRepository.(*postgres.Repository)
	if !ok {
		t.Fatal("archive fixture lacks PostgreSQL repository")
	}
	second := nativeOrganization{ID: "org_lost_archive", Name: "Second", Slug: "lost-archive"}
	seedPostgresHubAdmin(t, repository, f.service, second)
	fixtures := []nativeFixture{f.nativeFixture, newNativeFixture(t, f.service, second.ID, "archive example")}
	far := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name                                  string
		lease                                 string
		checkpoint, acknowledged, unpublished bool
		archivable                            bool
	}{
		{name: "live lease keeps running attempt", lease: "live"},
		{name: "expired lease releases lost attempt", lease: "expired", archivable: true},
		{name: "released lease releases lost attempt", lease: "released", archivable: true},
		{name: "checkpoint awaiting cleanup is retained", lease: "expired", checkpoint: true},
		{name: "acknowledged checkpoint cleanup releases lost attempt", lease: "expired", checkpoint: true, acknowledged: true, archivable: true},
		{name: "unresolved publication is retained", lease: "expired", unpublished: true},
	}
	for _, fixture := range fixtures {
		t.Run(string(fixture.project.OrganizationID), func(t *testing.T) {
			f.nativeFixture = fixture
			f.service.localOrganization, f.service.database.hostedOrganization, f.service.requestMetrics.Organization = string(fixture.project.OrganizationID), fixture.project.OrganizationID, fixture.project.OrganizationID
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			for index, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					issue := f.create(t, fmt.Sprintf("lost-%d", index))
					worker := f.worker(t, fmt.Sprintf("lost-%d", index))
					claimed := claimNativeAttempt(t, f.nativeFixture, worker, "machine", fmt.Sprintf("lost-%d", index), issue.WorkItemID)
					path := f.base + "/work-items/" + string(issue.WorkItemID)
					start := nativeStartedEvent(claimed)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
					if test.checkpoint {
						checkpoint := start
						checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence, checkpoint.Data.Handoff = "run.checkpointed", "checkpoint", 2, nativeTestCheckpoint()
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
					}
					current := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", worker, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done", LeaseID: claimed.ID, FencingToken: claimed.FencingToken}, ExpectedRevision: current.Revision, State: "Done", Reason: "worker_progress"}), http.StatusOK)
					f.service.tokenUseWork.Wait()
					lease := leasestore.Lease{LeaseID: string(claimed.ID), FencingToken: int64(claimed.FencingToken)}
					if test.lease == "released" {
						f.write(t, func(w tenantstore.Writer) error {
							_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released")
							return err
						})
					}
					f.clock.Add(int64(74 * time.Hour))
					switch test.lease {
					case "live":
						if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET expires_at=$3 WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, lease.LeaseID, far.AddDate(100, 0, 0)); err != nil {
							t.Fatal(err)
						}
					case "expired":
						if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=$3,renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, lease.LeaseID, far, far.Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
					if test.acknowledged {
						if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET cleanup_acknowledged_at=updated_at WHERE organization_id=$1 AND lease_id=$2`, f.project.OrganizationID, lease.LeaseID); err != nil {
							t.Fatal(err)
						}
					}
					if test.unpublished {
						seedRetentionChange(t, f, issue.WorkItemID, false, false)
					}
					current = readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
					var stored string
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						attempt, err := r.LatestAttempt(t.Context(), mustWorkItemRowID(t, r, f.project.ID, issue.WorkItemID))
						stored = attempt.Status
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if stored != "running" {
						t.Fatalf("fixture attempt status=%s, want a stored running attempt", stored)
					}
					eligible := storageArchiveEligible(t, f, issue.WorkItemID)
					if eligible != test.archivable {
						t.Fatalf("storage archive eligible=%t, want %t", eligible, test.archivable)
					}
					f.clock.Add(int64(31 * 24 * time.Hour))
					if err := f.service.maintainNativeRetention(t.Context(), f.now()); err != nil {
						t.Fatal(err)
					}
					got := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
					if got.Archived != test.archivable {
						t.Fatalf("auto archived=%t, want %t", got.Archived, test.archivable)
					}
					if !test.archivable && got.Revision != current.Revision {
						t.Fatalf("retained issue changed revision %d -> %d", current.Revision, got.Revision)
					}
				})
			}
		})
	}
}

func mustWorkItemRowID(t *testing.T, r tenantstore.Reader, project tracker.ProjectID, item tracker.NativeWorkItemID) int64 {
	t.Helper()
	row, err := r.WorkItem(t.Context(), project, item)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func storageArchiveEligible(t *testing.T, f *postgresStorageArchiveFixture, item tracker.NativeWorkItemID) bool {
	t.Helper()
	before := f.now().Add(-issueStorageArchiveAge)
	var eligible bool
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		reader := r.(attachmentstore.StorageArchiveReader)
		cursor := attachmentstore.StorageArchiveCursor{}
		for {
			page, err := reader.StorageArchiveCandidates(t.Context(), before, cursor, 4)
			if err != nil {
				return err
			}
			for _, candidate := range page.Items {
				if candidate.Stamp.WorkItemID != item {
					continue
				}
				_, err := reader.StorageArchiveSnapshot(t.Context(), candidate.Stamp, before, 1)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				eligible = err == nil
				return err
			}
			if page.Next == (attachmentstore.StorageArchiveCursor{}) {
				return fmt.Errorf("work item %s is not a storage archive candidate", item)
			}
			cursor = page.Next
		}
	}); err != nil {
		t.Fatal(err)
	}
	return eligible
}
