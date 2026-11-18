package accessrecords_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentReferenceBound(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, admin)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			pgstoretest.SeedTenantAccess(t, repository, organization)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES ($1,'prj_shared','attachment_shared','first','example.txt','text/plain',1,$2,$3)`, organization, strings.Repeat("a", 64), pgstoretest.HistoryAt()); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) SELECT $1,'comment_' || n,'prj_shared','wi_shared',n+1,$1 || '/body/' || n,'human','second','source_' || n FROM generate_series(1,201) AS n`, organization); err != nil {
				t.Fatal(err)
			}
			bind := func(comment string, retain bool) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "attachment_shared", WorkItemID: "wi_shared", CommentID: comment, RetainWithWorkItem: retain})
				})
			}
			for n := 1; n <= 200; n++ {
				if err := bind(fmt.Sprintf("comment_%d", n), false); err != nil {
					t.Fatalf("reference %d: %v", n, err)
				}
			}
			for _, tc := range []struct {
				name, comment string
				retain        bool
				want          error
			}{
				{name: "reject 201st comment", comment: "comment_201", want: attachment.ErrInvalid},
				{name: "retry existing reference", comment: "comment_1"},
				{name: "retain existing reference", comment: "comment_1", retain: true},
				{name: "reject additional issue reference", want: attachment.ErrInvalid},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := bind(tc.comment, tc.retain); !errors.Is(err, tc.want) {
						t.Errorf("bind error = %v, want %v", err, tc.want)
					}
				})
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				refs, err := r.AttachmentReferences(t.Context(), "prj_shared", "attachment_shared", 201)
				if err != nil {
					return err
				}
				if len(refs) != 200 {
					t.Errorf("attachment has %d references; metadata read bound is 200", len(refs))
				}
				for _, ref := range refs {
					if ref.CommentID == "comment_1" && !ref.RetainWithWorkItem {
						t.Error("existing reference retention was not updated")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAttachmentObjectLookupProtectsTenantHistory(t *testing.T) {
	r, pool := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	counter := &pgstoretest.ReportCountingTracer{}
	counted, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, counter), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	keys := map[tracker.OrganizationID]string{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, org)
		f := pgstoretest.SeedHistory(t, r, org, "lookup")
		keys[org] = f.Event.PayloadObjectKey
	}
	for org, key := range keys {
		if err := counted.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			for other, candidate := range keys {
				exists, err := reader.AttachmentObjectExists(t.Context(), "", candidate)
				if err != nil {
					return err
				}
				if exists != (other == org) {
					t.Errorf("history object ownership: tenant=%s object tenant=%s exists=%v", org, other, exists)
				}
			}
			orphan := strings.Replace(key, key[len(key)-64:], strings.Repeat("0", 64), 1)
			batch := make([]string, attachment.ReferenceBatchSize)
			for i := range batch {
				batch[i] = orphan
			}
			batch[0], batch[1] = keys["org_first"], keys["org_second"]
			before := counter.Count.Load()
			results, err := reader.ReferencedAttachmentObjects(t.Context(), batch)
			if err != nil {
				return err
			}
			t.Logf("%s: %d reference statements for %d swept keys", org, counter.Count.Load()-before, len(batch))
			if got := counter.Count.Load() - before; got != 1 {
				t.Errorf("%d reference statements for %d keys, want one", got, len(batch))
			}
			if len(results) != len(batch) {
				t.Fatalf("got %d results for %d keys", len(results), len(batch))
			}
			for i, exists := range results {
				if exists != (batch[i] == key) {
					t.Errorf("tenant %s: reference result at %d = %v", org, i, exists)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttachmentReferencesFollowTheirSourceText(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, admin)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			pgstoretest.SeedTenantAccess(t, repository, organization)
			for _, id := range []string{"att_edit", "att_shared", "att_retained", "att_late"} {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES ($1,'prj_shared',$2,'first','example.png','image/png',1,$3,$4)`, organization, id, strings.Repeat("a", 64), pgstoretest.HistoryAt()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) VALUES ($1,'comment_peer','prj_shared','wi_shared',900,$1 || '/body/peer','human','second','source_peer')`, organization); err != nil {
				t.Fatal(err)
			}
			write := func(update func(w tenantstore.Writer) error) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, update)
			}
			replace := func(previous, current []string) error {
				return write(func(w tenantstore.Writer) error {
					return w.ReplaceAttachmentReferences(t.Context(), "prj_shared", "wi_shared", "", previous, current)
				})
			}
			bound := func(id string) []string {
				t.Helper()
				var sources []string
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					refs, err := r.AttachmentReferences(t.Context(), "prj_shared", id, 10)
					for _, ref := range refs {
						sources = append(sources, fmt.Sprintf("%s/%s/%t", ref.WorkItemID, ref.CommentID, ref.RetainWithWorkItem))
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return sources
			}
			if err := write(func(w tenantstore.Writer) error {
				if err := w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "att_shared", WorkItemID: "wi_shared", CommentID: "comment_peer"}); err != nil {
					return err
				}
				return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "att_retained", WorkItemID: "wi_shared", RetainWithWorkItem: true})
			}); err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				name              string
				previous, current []string
				deleteFirst       string
				wantErr           error
				check             string
				want              []string
			}{
				{name: "edit adds a readable attachment", current: []string{"att_edit", "att_edit"}, check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "replayed edit keeps one binding", previous: []string{"att_edit"}, current: []string{"att_edit"}, check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "unchanged reference survives deletion without new access", previous: []string{"att_edit"}, current: []string{"att_edit"}, deleteFirst: "att_edit", check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "removal unbinds only this source", previous: []string{"att_shared"}, current: nil, check: "att_shared", want: []string{"wi_shared/comment_peer/false"}},
				{name: "removal keeps explicit retention", previous: []string{"att_retained"}, current: nil, check: "att_retained", want: []string{"wi_shared//true"}},
				{name: "removing a deleted reference unbinds it", previous: []string{"att_edit"}, current: nil, check: "att_edit", want: nil},
				{name: "re-adding after deletion is refused", current: []string{"att_edit"}, wantErr: corestore.ErrNotFound, check: "att_edit", want: nil},
				{name: "deletion between preparation and commit is refused", current: []string{"att_late"}, deleteFirst: "att_late", wantErr: corestore.ErrNotFound, check: "att_late", want: nil},
			} {
				if step.deleteFirst != "" {
					if _, err := admin.ExecContext(t.Context(), `UPDATE attachments SET deleted_at=$3 WHERE organization_id=$1 AND project_id='prj_shared' AND id=$2`, organization, step.deleteFirst, pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
				}
				if err := replace(step.previous, step.current); !errors.Is(err, step.wantErr) {
					t.Fatalf("%s: error=%v want %v", step.name, err, step.wantErr)
				}
				if got := bound(step.check); fmt.Sprint(got) != fmt.Sprint(step.want) {
					t.Fatalf("%s: bindings=%v want %v", step.name, got, step.want)
				}
			}
			var deleted int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE organization_id=$1 AND id IN ('att_shared','att_retained') AND deleted_at IS NOT NULL`, organization).Scan(&deleted); err != nil || deleted != 0 {
				t.Fatalf("unbinding deleted attachment rows=%d error=%v", deleted, err)
			}
		})
	}
}

func TestDetailStoreReclaimsOnlyUnreferencedExpiredObjects(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	_, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	counter := &pgstoretest.ReportCountingTracer{}
	repository, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, counter), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start
	store := repository.DetailStore(func() time.Time { return now })
	organizations := []tracker.OrganizationID{"org_reclaim_first", "org_reclaim_second"}
	type objects struct{ referenced, abandoned, touched, recent attachment.ArchiveReference }
	stored := map[tracker.OrganizationID]*objects{}
	put := func(org tracker.OrganizationID, body string) attachment.ArchiveReference {
		t.Helper()
		ref, err := store.StoreDetail(t.Context(), string(org), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	present := func(org tracker.OrganizationID, ref attachment.ArchiveReference) bool {
		t.Helper()
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2`, org, ref.Key).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}
	for _, org := range organizations {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: start}); err != nil {
				return err
			}
			return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1})
		}); err != nil {
			t.Fatal(err)
		}
		set := &objects{referenced: put(org, `{"kind":"referenced"}`), abandoned: put(org, `{"kind":"abandoned"}`), touched: put(org, `{"kind":"touched"}`)}
		ref := set.referenced
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO archive_objects(organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,created_at) VALUES($1,'archive_reclaim',$2,$3,$4,$5,$6,$7,$8,$9,$10)`, org, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, start); err != nil {
			t.Fatal(err)
		}
		stored[org] = set
	}
	now = start.Add(8 * 24 * time.Hour)
	for _, org := range organizations {
		stored[org].recent = put(org, `{"kind":"recent"}`)
		if touched := put(org, `{"kind":"touched"}`); touched != stored[org].touched {
			t.Fatalf("re-store changed reference: %+v", touched)
		}
	}
	cutoff := now.Add(-7 * 24 * time.Hour)
	reclaimed, err := store.Reclaim(t.Context(), organizations[0], cutoff)
	if err != nil || reclaimed != 1 {
		t.Fatalf("first reclaim=%d err=%v", reclaimed, err)
	}
	first, second := stored[organizations[0]], stored[organizations[1]]
	if present(organizations[0], first.abandoned) || !present(organizations[0], first.referenced) || !present(organizations[0], first.touched) || !present(organizations[0], first.recent) {
		t.Fatal("reclaim removed a referenced, re-stored or recent object, or kept the abandoned one")
	}
	if !present(organizations[1], second.abandoned) {
		t.Fatal("reclaim of one organization removed another organization's object")
	}
	for _, org := range organizations {
		counter.Count.Store(0)
		n, err := store.Reclaim(t.Context(), org, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if org == organizations[0] && (n != 0 || counter.Count.Load() != 2) {
			t.Fatalf("idle reclaim deleted %d and ran %d statements, want only tenant binding and due probe", n, counter.Count.Load())
		}
	}
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM archive_objects WHERE organization_id=$1 AND id='archive_reclaim'`, organizations[0]); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := store.Reclaim(t.Context(), organizations[0], cutoff); err != nil || reclaimed != 1 || present(organizations[0], first.referenced) {
		t.Fatalf("expired reference kept its bytes: reclaimed=%d err=%v", reclaimed, err)
	}
	readPage := func(org tracker.OrganizationID) attachment.ReclaimPage {
		t.Helper()
		var page attachment.ReclaimPage
		if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			page, err = r.ArchiveReclaimPage(t.Context(), now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := readPage(organizations[0])
	if len(page.Changed) != 1 || page.Changed[0].Key != first.referenced.Key || len(readPage(organizations[1]).Changed) != 0 {
		t.Fatalf("reference loss escaped its tenant: %+v", page)
	}
	ack := func(receipt attachment.ReclaimReceipt) {
		t.Helper()
		if err := repository.WriteTenant(t.Context(), organizations[0], corestore.Background, func(w tenantstore.Writer) error { return w.CompleteArchiveReclaim(t.Context(), receipt) }); err != nil {
			t.Fatal(err)
		}
	}
	receipt := attachment.ReclaimReceipt{Next: first.referenced.Key, Checked: page.Changed}
	later := now.Add(time.Hour)
	receipt.Checked[0].Due = &later
	ack(receipt)
	if got := readPage(organizations[0]); len(got.Changed) != 0 || got.After != receipt.Next {
		t.Fatalf("premature retry or lost cursor: %+v", got)
	}
	now = later
	if got := readPage(organizations[0]); len(got.Changed) != 1 || got.Changed[0].Key != first.referenced.Key {
		t.Fatalf("deferred key lost: %+v", got)
	}
	receipt.After, receipt.Next = receipt.Next, ""
	receipt.Checked[0].Due = nil
	receipt.Checked[0].Revision++
	ack(receipt)
	if len(readPage(organizations[0]).Changed) != 1 {
		t.Fatal("stale receipt cleared a different revision")
	}
	receipt.Checked[0].Revision--
	ack(receipt)
	if len(readPage(organizations[0]).Changed) != 0 {
		t.Fatal("completed candidate was replayed")
	}
	budget := postgres.DetailReclaimPage * postgres.DetailReclaimPages
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO hub_detail_objects(organization_id,object_key,sha256,compressed_bytes,content_bytes,content,created_at)
SELECT $1,'orgs/'||$1||'/blobs/sha256/aa/'||lpad(to_hex(n),64,'0'),lpad(to_hex(n),64,'0'),1,1,'\x00'::bytea,$2 FROM generate_series(1,$3::int) AS n`, organizations[1], start, budget+1); err != nil {
		t.Fatal(err)
	}
	counter.Count.Store(0)
	firstPass, err := store.Reclaim(t.Context(), organizations[1], cutoff)
	if err != nil || firstPass == 0 || firstPass > budget {
		t.Fatalf("first bounded reclaim=%d err=%v, want 1..%d", firstPass, err, budget)
	}
	if got := counter.Count.Load(); got > postgres.DetailReclaimPages*3 {
		t.Errorf("full reclaim pass used %d statements for %d objects, want at most three per page including tenant binding", got, firstPass)
	}
	if secondPass, err := store.Reclaim(t.Context(), organizations[1], cutoff); err != nil || firstPass+secondPass != budget+1 {
		t.Fatalf("continued reclaim=%d+%d err=%v, want %d abandoned objects", firstPass, secondPass, err, budget+1)
	}
	if !present(organizations[1], second.referenced) || !present(organizations[1], second.touched) || !present(organizations[1], second.recent) {
		t.Fatal("bounded reclaim removed a live object")
	}
}
