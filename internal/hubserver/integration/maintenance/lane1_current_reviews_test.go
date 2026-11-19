package maintenance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type currentReviewUnavailableObject struct {
	attachment.DetailStore
	key       string
	attempted chan struct{}
}

func (s currentReviewUnavailableObject) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Key == s.key {
		select {
		case s.attempted <- struct{}{}:
		default:
		}
		return nil, os.ErrNotExist
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestPostgresMaintenanceRebuildsCurrentReviewsPastUnavailableChange(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	details := hubtest.NewDetails()
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		t.Run(string(organization), func(t *testing.T) {
			cfg := hubfixture.StartupConfig()
			cfg.RunDetails = details
			cfg.Hosted.OrganizationID = string(organization)
			cfg.Hosted.WorkOSOrganizationID = "provider_" + string(organization)
			cfg.Clock, cfg.Logger = time.Now, hubfixture.DiscardLogger()
			cfg.InitialAdminToken, cfg.SecretKeys = []byte(hubfixture.AdminToken), hubfixture.SecretKeys(t)
			cfg.WebhookMaintenanceInterval = time.Hour
			connection := hubtest.PostgresConfig(t, admin)
			cfg.Postgres, cfg.DatabaseBackend = &connection, hubserver.DatabasePostgres
			cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = repository, repository, repository
			now := cfg.Clock().UTC().Truncate(time.Microsecond)
			deployment, generation := cfg.Hosted.Deployment()
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-"), CreatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation})
			}); err != nil {
				t.Fatal(err)
			}
			hubfixture.SeedAdmin(t, repository, organization, hubfixture.AdminToken, now)
			project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{ID: "prj_review", Name: "Review project"})
			approved := hubtest.Policy(t, repository, cfg.RunDetails, project, hubtest.PolicyOptions{Now: now})
			issue := hubtest.Issue(t, repository, cfg.RunDetails, project, hubtest.IssueOptions{Title: "Review recovery", Body: "## Acceptance criteria\nRecover review evidence.\n## Must not break\nLeave unavailable evidence pending.\n## How we know it worked\nRead the persisted review summary.", Now: now}).Issue
			content := eventstore.NewContent(cfg.RunDetails)
			unavailable := seedPendingCurrentReview(t, repository, content, issue, "a_unavailable", approved.Descriptor.ID, now)
			seedPendingCurrentReview(t, repository, content, issue, "b_available", approved.Descriptor.ID, now)
			attempted := make(chan struct{}, 1)
			cfg.RunDetails = currentReviewUnavailableObject{DetailStore: cfg.RunDetails, key: unavailable, attempted: attempted}
			cfg = hubserver.WithDeps(cfg, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
				<-ctx.Done()
				return ctx.Err()
			}})
			service, err := hubserver.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			hosted := &hubfixture.Hosted{Hub: &hubfixture.Hub{Service: service, Config: cfg, Repository: repository, Primary: organization}, Provider: cfg.Hosted.Provider.(*hubfixture.HostedProvider)}
			owner := hosted.User(t, "review_maintenance_owner", "owner", "review-maintenance@example.test", "", "")
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-attempted:
					return nil
				default:
					return errors.New("startup maintenance did not reach the unavailable review")
				}
			})
			path := hubfixture.ProjectBase(organization, project.ID) + "/work-items/" + string(issue.WorkItemID)
			testenv.WaitUntil(t, t.Context(), func() error {
				var bad, good eventstore.Change
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					if bad, err = r.Change(t.Context(), "a_unavailable"); err != nil {
						return err
					}
					good, err = r.Change(t.Context(), "b_available")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if bad.ReviewSummarySequence != 0 {
					t.Fatal("maintenance completed a Change with unavailable review evidence")
				}
				if good.ReviewSummarySequence != 0 && good.ReviewSummarySequence == good.EvidenceSequence {
					return nil
				}
				refresh := hubapi.NewID("refresh")
				update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: refresh}, ExpectedRevision: issue.Revision, Title: new("Review refresh " + refresh)}
				response := hosted.Request(t, owner, http.MethodPatch, path, update)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				hubfixture.Decode(t, response, &issue)
				return errors.New("the available review summary is still pending")
			})
		})
	}
}

func seedPendingCurrentReview(t *testing.T, repository tenantstore.Repository, content *eventstore.Content, issue tracker.NativeIssue, id, policyID string, now time.Time) string {
	t.Helper()
	organization := issue.OrganizationID
	change := tracker.ChangeRequest{ID: id, OrganizationID: organization, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{issue.WorkItemID}, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
	row, err := content.PrepareChange(t.Context(), organization, maintenanceReviewJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	version := tracker.ChangeVersion{ID: "version_" + id, ChangeID: id, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: policyID, HeadSHA: strings.Repeat("b", 40)}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now}
	versionRow, err := content.PrepareVersion(t.Context(), organization, maintenanceReviewJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	review := tracker.ChangeReview{ID: "review_" + id, VersionID: version.ID, Decision: "approved", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now}
	evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: organization, ChangeID: id, Kind: "review"}, maintenanceReviewJSON(t, review))
	if err != nil {
		t.Fatal(err)
	}
	evidence.ReviewKind, evidence.ReviewSessionKnown = "", false
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), row); err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
			return err
		}
		row.CurrentVersionID = &version.ID
		if _, err := w.UpdateChange(t.Context(), row); err != nil {
			return err
		}
		_, err := w.AppendEvidence(t.Context(), evidence)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return evidence.PayloadObjectKey
}

func maintenanceReviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
