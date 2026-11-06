package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseauditstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseAuditTypedOwner(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		var latest releaseaudit.Checkpoint
		for _, sequence := range []int{2, 1} {
			snapshot := releaseaudit.Snapshot{Identity: releaseaudit.Identity{ProjectID: "prj_shared", Repository: "example-org/" + string(org), Commit: strings.Repeat("a", 40), ReleaseSeries: "release/v1.0", RunID: "run", Attempt: "1", JobID: "audit", RunURL: "https://example.test/run"}, Sequence: sequence, Status: "incomplete", BundleCommit: releaseaudit.BundleCommit, BundleDigest: releaseaudit.BundleDigest, Allowance: releaseaudit.Allowance{Invocations: 20, Usage: releaseaudit.Usage{Input: 1000, Output: 1000, Cached: 1000}}, Ledger: []byte(`[]`)}
			raw, c, err := releaseaudit.SnapshotBytes(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("att_%d", sequence)
			c.Parts = []releaseaudit.Part{{AttachmentID: id, Index: 0, Offset: 0, Bytes: len(raw), Digest: c.Digest}}
			if _, err := admin.ExecContext(ctx, `INSERT INTO attachments(organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES($1,'prj_shared',$2,'first','audit.txt','text/plain',$3,$4,$5)`, org, id, len(raw), c.Digest, historyAt()); err != nil {
				t.Fatal(err)
			}
			put := func(checkpoint releaseaudit.Checkpoint) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					owner, ok := w.(releaseauditstore.Writer)
					if !ok {
						t.Fatal("PostgreSQL release audit owner unavailable")
					}
					return owner.PutReleaseAudit(ctx, "prj_shared", checkpoint, historyAt())
				})
			}
			invalid := c
			invalid.Parts = append([]releaseaudit.Part(nil), c.Parts...)
			invalid.Parts[0].Digest = strings.Repeat("b", 64)
			if err := put(invalid); !errors.Is(err, eventstore.ErrInvalidRecord) {
				t.Fatalf("invalid attachment=%v", err)
			}
			for n := 0; n < 2; n++ {
				if err := put(c); err != nil {
					t.Fatalf("publish/replay: %v", err)
				}
			}
			changed := c
			changed.Status = "clean"
			if err := put(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("changed replay=%v", err)
			}
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				expired, err := w.(attachmentstore.ExpiryWriter).ExpireAttachment(ctx, "prj_shared", id, historyAt().Add(time.Hour), historyAt().Add(2*time.Hour))
				if expired {
					t.Error("checkpoint evidence expired")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(attachmentstore.DeletionWriter).DeleteAttachment(ctx, "prj_shared", id, historyAt())
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("checkpoint evidence deletion=%v", err)
			}
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.(releaseauditstore.Reader).ReleaseAudit(ctx, "prj_shared", c.ID)
				if err == nil && !reflect.DeepEqual(got, c) {
					t.Errorf("checkpoint round trip=%+v want=%+v", got, c)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if c.Sequence > latest.Sequence {
				latest = c
			}
		}
		if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			owner := r.(releaseauditstore.Reader)
			page, err := owner.ReleaseAudits(ctx, "prj_shared", "")
			if err != nil {
				return err
			}
			if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], latest) {
				t.Fatalf("latest checkpoint=%+v", page)
			}
			_, err = owner.ReleaseAudit(ctx, "prj_missing", latest.ID)
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("project isolation=%v", err)
			}
			page, err = owner.ReleaseAudits(ctx, "prj_shared", latest.ID)
			if err == nil && len(page.Items) != 0 {
				t.Error("cursor returned earlier rows")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
