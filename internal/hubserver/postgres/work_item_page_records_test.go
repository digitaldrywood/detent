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
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
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
