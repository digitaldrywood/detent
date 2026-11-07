package postgres_test

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPageRejectsStaleEvidence(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	var first healthstore.Finding
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		_, err := admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) VALUES($1,'finding','fingerprint','signal','class','project','prj_first',now(),now(),'attention','summary','inspect','','',0,0)`, org)
		if err != nil {
			t.Fatal(err)
		}
		var candidate healthstore.Finding
		err = repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			candidate, err = r.(healthstore.PageMaintainer).NextHealthPage(t.Context())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if org == "org_first" {
			first = candidate
		}
		if org == "org_second" {
			err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				_, err := w.(healthstore.PageMaintainer).PublishHealthPage(t.Context(), first, healthstore.Evidence{})
				return err
			})
			if !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("cross tenant publication = %v", err)
			}
		}
		_, err = admin.ExecContext(t.Context(), `UPDATE health_findings SET evidence_object_key='changed' WHERE organization_id=$1 AND id='finding'`, org)
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			owner := w.(healthstore.PageMaintainer)
			published, err := owner.PublishHealthPage(t.Context(), candidate, healthstore.Evidence{Reason: "stale"})
			if err != nil {
				return err
			}
			if published {
				t.Fatal("stale source was published")
			}
			next, err := owner.NextHealthPage(t.Context())
			if err != nil {
				return err
			}
			if next.Generation != candidate.Generation+1 || next.Evidence.Key != "changed" {
				t.Fatalf("lost source generation: %+v", next)
			}
			for range 2 {
				published, err = owner.PublishHealthPage(t.Context(), next, healthstore.Evidence{Reason: "current"})
				if err != nil {
					return err
				}
				if !published {
					t.Fatal("idempotent publication rejected")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
