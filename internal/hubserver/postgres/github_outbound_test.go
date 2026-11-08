package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubOutboundFencesExpiredClaimsAcrossTenants(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		key, err := attachment.ArchiveKey(string(org), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		payload := eventstore.Receipt{OrganizationID: org, PayloadObjectKey: key, PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadBytes: 1, PayloadContentBytes: 1}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"one", "two"} {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
					return err
				}
				out := projectstore.GitHubOutbound{ProjectID: project, WorkItemID: "wi_example", Key: string(project), Repository: "example-org/example-project", IssueNumber: 1, Kind: "workpad", CreatedAt: now, Payload: payload}
				for range 2 {
					if err := w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), out); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var expired projectstore.GitHubOutbound
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubOutboxWriter)
			var err error
			expired, err = owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "old")
			if err != nil {
				return err
			}
			if expired.Key != "one" || expired.Payload.OrganizationID != org {
				t.Fatalf("first claim=%+v", expired)
			}
			second, err := owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "second")
			if err != nil {
				return err
			}
			if second.Key != "two" {
				t.Fatalf("unexpired claim was reclaimed: %+v", second)
			}
			second.Status = "completed"
			second.Outcome = "committed"
			return owner.FinishGitHubOutbound(t.Context(), second, now)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubOutboxWriter)
			if err := owner.RenewGitHubOutbound(t.Context(), expired.ID, expired.LeaseID, now.Add(30*time.Second), now.Add(2*time.Minute), true); err != nil {
				return err
			}
			if _, err := owner.ClaimGitHubOutbound(t.Context(), now.Add(time.Minute), now.Add(3*time.Minute), "early"); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("renewed claim was reclaimed: %v", err)
			}
			current, err := owner.ClaimGitHubOutbound(t.Context(), now.Add(2*time.Minute), now.Add(3*time.Minute), "current")
			if err != nil {
				return err
			}
			if current.ID != expired.ID || current.Attempts != 2 || current.Outcome != "uncertain" {
				t.Fatalf("recovered=%+v", current)
			}
			expired.Status = "completed"
			expired.Outcome = "committed"
			if err := owner.FinishGitHubOutbound(t.Context(), expired, now.Add(2*time.Minute)); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("expired claim completed: %v", err)
			}
			current.Status = "completed"
			current.Outcome = "committed"
			if err := owner.RenewGitHubOutbound(t.Context(), expired.ID, expired.LeaseID, now.Add(2*time.Minute), now.Add(4*time.Minute), false); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("old claim renewed: %v", err)
			}
			return owner.FinishGitHubOutbound(t.Context(), current, now.Add(2*time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), now.Add(3*time.Minute), now.Add(4*time.Minute), "none")
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("completed command was reclaimed: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
