package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type currentReviewMaintenanceStep struct {
	organization tracker.OrganizationID
	id           string
	count        int
	err          error
}

type currentReviewMaintenanceRepository struct {
	tenantstore.Repository
	owner interface {
		BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
	}
	mu    sync.Mutex
	steps []currentReviewMaintenanceStep
}

func (r *currentReviewMaintenanceRepository) BackfillCurrentChangeReviewsAfter(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int, after string) (int, string, error) {
	n, id, err := r.owner.BackfillCurrentChangeReviewsAfter(ctx, organization, content, limit, after)
	r.mu.Lock()
	r.steps = append(r.steps, currentReviewMaintenanceStep{organization: organization, id: id, count: n, err: err})
	r.mu.Unlock()
	return n, id, err
}

func (r *currentReviewMaintenanceRepository) snapshot() []currentReviewMaintenanceStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]currentReviewMaintenanceStep(nil), r.steps...)
}

type currentReviewUnavailableObject struct {
	attachment.DetailStore
	key string
}

func (s currentReviewUnavailableObject) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Key == s.key {
		return nil, os.ErrNotExist
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestPostgresMaintenanceRebuildsCurrentReviewsPastUnavailableChange(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	objects := &memoryRunDetails{}
	services := map[tracker.OrganizationID]*Service{}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
		cfg.Hosted.OrganizationID = string(organization)
		cfg.Hosted.WorkOSOrganizationID = "provider_" + string(organization)
		cfg.RunDetails = objects
		cfg.WebhookMaintenanceInterval = time.Hour
		service, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		seedStartupOrganization(t, service.config.TenantRepository, organization)
		content := eventstore.NewContent(objects)
		now := time.Now().UTC().Truncate(time.Microsecond)
		var unavailable string
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_review", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_review", NativeID: "wi_review", Number: 1, Revision: 1, Title: "Example", WorkflowStateName: new("Todo")})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a_unavailable", "b_available"} {
			change := tracker.ChangeRequest{ID: id, OrganizationID: organization, ProjectID: "prj_review", WorkItemID: "wi_review", LinkedIssues: []tracker.NativeWorkItemID{"wi_review"}, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
			raw, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			row, err := content.PrepareChange(t.Context(), organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			version := tracker.ChangeVersion{ID: "version_" + id, ChangeID: id, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: "example-policy", HeadSHA: strings.Repeat("b", 40)}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now}
			raw, err = json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			versionRow, err := content.PrepareVersion(t.Context(), organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			review := tracker.ChangeReview{ID: "review_" + id, VersionID: version.ID, Decision: "approved", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now}
			raw, err = json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: organization, ChangeID: id, Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			evidence.ReviewKind, evidence.ReviewSessionKnown = "", false
			if id == "a_unavailable" {
				unavailable = evidence.PayloadObjectKey
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateChange(t.Context(), row); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				row.CurrentVersionID = new(version.ID)
				if _, err := w.UpdateChange(t.Context(), row); err != nil {
					return err
				}
				_, err := w.AppendEvidence(t.Context(), evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		service.config.RunDetails = currentReviewUnavailableObject{DetailStore: objects, key: unavailable}
		services[organization] = service
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		t.Run(string(organization), func(t *testing.T) {
			service := services[organization]
			owner := service.config.TenantRepository.(interface {
				BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
			})
			probe := &currentReviewMaintenanceRepository{Repository: service.config.TenantRepository, owner: owner}
			service.config.TenantRepository = probe
			defer func() { service.config.TenantRepository = probe.Repository }()
			oldDone := service.workerDone
			service.workerDone = make(chan struct{})
			defer func() { service.workerDone = oldDone }()
			synctest.Test(t, func(t *testing.T) {
				previousRefresh := service.boardRefresh
				service.boardRefresh = make(chan struct{}, 1)
				defer func() { service.boardRefresh = previousRefresh }()
				ctx, cancel := context.WithCancel(t.Context())
				done := service.workerDone
				defer func() { cancel(); <-done }()
				go service.runPostgresMaintenance(ctx)
				synctest.Wait()
				steps := probe.snapshot()
				if len(steps) != 1 || steps[0].organization != organization || steps[0].id != "a_unavailable" || steps[0].err == nil {
					t.Fatalf("actual PostgreSQL loop did not attempt the first pending Change: %+v", steps)
				}
				service.boardRefresh <- struct{}{}
				synctest.Wait()
				steps = probe.snapshot()
				if len(steps) != 2 || steps[1].id != "b_available" || steps[1].count != 1 || steps[1].err != nil {
					t.Fatalf("unavailable Change starved later work: %+v", steps)
				}
				if err := probe.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
					bad, err := r.Change(ctx, "a_unavailable")
					if err != nil {
						return err
					}
					good, err := r.Change(ctx, "b_available")
					if err != nil {
						return err
					}
					if bad.ReviewSummarySequence != 0 || good.ReviewSummarySequence != good.EvidenceSequence || good.ReviewSummarySequence != 1 {
						return errors.New("maintenance falsely completed the bad Change or lost the healthy Change")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
