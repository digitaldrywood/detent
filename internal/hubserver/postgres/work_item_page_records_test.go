package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemPageModelFencesReplayAndTenants(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)

		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(viewstore.WorkItemPageReader).CommentPageRecords(t.Context(), "prj_first", "wi_first_1", 0, false, 2)
			if !errors.Is(err, viewstore.ErrPagePending) {
				t.Fatalf("cold comments completed successfully: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var candidate viewstore.PageCandidate
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			candidate, err = r.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		original := viewstore.ItemPageRecord{Generation: candidate.Generation, SourceAt: candidate.SourceAt, ProjectedAt: historyAt(), Value: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: candidate.ProjectID, WorkItemID: candidate.WorkItemID, Number: 1, Revision: 1, Profile: "native"}, Title: string(org), Body: "Current body", Actor: tracker.Actor{Kind: "human", PrincipalID: "first"}, Labels: []string{"example"}, Assignees: []string{}, CreatedAt: historyAt(), UpdatedAt: historyAt(), LastActivityAt: historyAt(), ReportContext: &tracker.ReportContext{URL: "https://example.test"}, Verification: &tracker.NativeVerification{State: "pending"}}}
		publish := func(record viewstore.ItemPageRecord) error {
			return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(viewstore.WorkItemPageWriter).PublishWorkItemPage(t.Context(), candidate, &record, nil)
			})
		}
		wrong := original
		wrong.Value.OrganizationID = "org_other"
		if err := publish(wrong); !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("cross tenant publish=%v", err)
		}
		if err := publish(original); err != nil {
			t.Fatal(err)
		}
		replay := original
		replay.Value.Title = "Replay must not replace"
		if err := publish(replay); err != nil {
			t.Fatal(err)
		}

		update := func(title string) error {
			return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.WorkItem(t.Context(), candidate.ProjectID, candidate.WorkItemID)
				if err != nil {
					return err
				}
				row.Title = title
				_, err = w.UpdateWorkItem(t.Context(), row)
				return err
			})
		}
		if err := update("First source update"); err != nil {
			t.Fatal(err)
		}
		var stale viewstore.PageCandidate
		for step := 0; step < 16; step++ {
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				stale, err = r.(viewstore.WorkItemPageReader).NextWorkItemPageCandidate(t.Context())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if stale.Kind == "item" && stale.WorkItemID == candidate.WorkItemID {
				break
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(viewstore.WorkItemPageWriter).AdvanceWorkItemPageCursor(t.Context(), stale)
			}); err != nil {
				t.Fatal(err)
			}
			if step == 15 {
				t.Fatal("fixture source not reached")
			}
		}
		if err := update("Second source update"); err != nil {
			t.Fatal(err)
		}
		obsolete := original
		obsolete.Generation = stale.Generation
		obsolete.Value.Title = "Obsolete projection"
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(viewstore.WorkItemPageWriter).PublishWorkItemPage(t.Context(), stale, &obsolete, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			revision, err := r.(viewstore.WorkItemPageRevisionReader).WorkItemPageProjectionRevision(t.Context())
			if err != nil {
				return err
			}
			if revision != 1 {
				t.Fatalf("publication watermark=%d, want one after replay and stale publication", revision)
			}
			got, err := r.(viewstore.WorkItemPageReader).WorkItemDetailPage(t.Context(), candidate.ProjectID, candidate.WorkItemID)
			if err != nil {
				return err
			}
			if got.Value.Title != string(org) || got.Value.Body != "Current body" || got.Value.ReportContext == nil || got.Value.ReportContext.URL != "https://example.test" || got.Value.Verification == nil || got.Value.Verification.State != "pending" || got.Generation != original.Generation {
				t.Fatalf("projection=%+v", got)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(context.Background(), "org_other", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(viewstore.WorkItemPageReader).WorkItemDetailPage(t.Context(), "prj_first", "wi_first_1")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("unrelated tenant read=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
