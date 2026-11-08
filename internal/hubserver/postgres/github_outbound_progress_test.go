package postgres_test

import (
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

func TestGitHubOutboundVerificationKeepsLatestScopedFacts(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	for _, organization := range []tracker.OrganizationID{"first", "second"} {
		key, err := attachment.ArchiveKey(string(organization), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: string(organization), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"one", "two"} {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}); err != nil {
					return err
				}
				payload := eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: key, PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadBytes: 1, PayloadContentBytes: 1}
				if err := w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: project, Key: string(project), Repository: "example-org/example-project", IssueNumber: 1, Kind: "merge_pull_request", CreatedAt: now, Payload: payload}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubOutboxWriter)
				row, err := owner.ClaimGitHubOutbound(t.Context(), now, now.Add(time.Minute), "lease_"+command)
				if err != nil {
					return err
				}
				if row.Key != command {
					t.Fatalf("claimed=%s want=%s", row.Key, command)
				}
				row.Progress = projectstore.GitHubOutboundProgress{Stage: "verify", Verification: 1, SourceUpdatedAt: now}
				row.Observations = []projectstore.GitHubOutboundObservation{
					{Kind: "review", Key: "reviewer", Sequence: 1, Ready: false}, {Kind: "review", Key: "reviewer", Sequence: 2, Ready: true},
					{Kind: "check", Key: "test", Sequence: 10, Ready: true}, {Kind: "check", Key: "test", Sequence: 2, Ready: false},
					{Kind: "status", Key: "build", Sequence: -1, Ready: true}, {Kind: "status", Key: "build", Sequence: -2, Ready: false},
				}
				if command == "one" {
					row.Observations = append(row.Observations, projectstore.GitHubOutboundObservation{Kind: "review", Key: "blocker", Sequence: 1, Ready: false})
				}
				row.Status, row.NextAttemptAt = "pending", now.Add(time.Hour)
				return owner.FinishGitHubOutbound(t.Context(), row, now)
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, command := range []string{"one", "two"} {
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), command)
				if err == nil && row.Blocked != (command == "one") {
					t.Fatalf("tenant=%s command=%s blocked=%t", organization, command, row.Blocked)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, command := range []string{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubOutboxWriter)
				row, err := owner.ClaimGitHubOutbound(t.Context(), now.Add(time.Hour), now.Add(time.Hour+time.Minute), "current_"+command)
				if err != nil {
					return err
				}
				row.Progress.Verification = 2
				row.Observations = []projectstore.GitHubOutboundObservation{{Kind: "review", Key: "reviewer", Sequence: 1, Ready: true}}
				row.Status, row.Outcome = "completed", "committed"
				return owner.FinishGitHubOutbound(t.Context(), row, now.Add(time.Hour))
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), command)
				if err == nil && (row.Blocked || row.Progress.Verification != 2) {
					t.Fatalf("old verification blocked current source: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
