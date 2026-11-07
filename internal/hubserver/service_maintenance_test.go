package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type maintenanceBatchProbe struct {
	*memoryRunDetails
	batches int
}

func (s *maintenanceBatchProbe) ReadDetails(ctx context.Context, org string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	s.batches++
	return attachment.ReadDetailBatch(ctx, org, refs, s.ReadDetail)
}

func TestHostedMaintenancePreservesRequestBatchReads(t *testing.T) {
	store := &maintenanceBatchProbe{memoryRunDetails: &memoryRunDetails{}}
	ref, err := store.StoreDetail(t.Context(), "org_example", bytes.Repeat([]byte("example"), 10))
	if err != nil {
		t.Fatal(err)
	}
	cfg := (Config{DatabaseBackend: DatabasePostgres, RunDetails: store}).normalized()
	batch, ok := cfg.RunDetails.(attachment.DetailBatchStore)
	if !ok {
		t.Fatal("normalization removed batched request reads")
	}
	for _, remaining := range []int64{0, ref.CompressedSize + ref.UncompressedSize} {
		ctx := t.Context()
		if remaining > 0 {
			ctx = (hubmaintenance.Budget{Items: 1, Bytes: remaining}).Context(ctx, hubmaintenance.Tenant{Organization: "org_example", Generation: 1})
		}
		rows, err := batch.ReadDetails(ctx, "org_example", []attachment.ArchiveReference{ref})
		if err != nil || len(rows) != 1 {
			t.Fatalf("batch read: rows=%d err=%v", len(rows), err)
		}
		if remaining > 0 {
			if _, err := batch.ReadDetails(ctx, "org_example", []attachment.ArchiveReference{ref}); err == nil {
				t.Fatal("exhausted batch budget accepted")
			}
		}
	}
	if store.batches != 2 {
		t.Fatalf("batched I/O calls=%d want=2", store.batches)
	}
}

func TestHostedMaintenancePublishesPagesForTwoTenants(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	cfg.WebhookMaintenanceInterval = time.Hour
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now().UTC().Truncate(time.Microsecond)
	tenants := []tracker.OrganizationID{"org_first", "org_second"}
	for _, org := range tenants {
		seedStartupOrganization(t, store.postgres, org)
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example", Number: 1, Revision: 1}, Title: "Example", Body: string(org), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(cfg.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_example", RecordID: "wi_example", Revision: 1}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: "wi_example", Number: 1, Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, org := range tenants {
		t.Run(string(org), func(t *testing.T) {
			started := make(chan *hubmaintenance.Scheduler, 2)
			cfg.Hosted.OrganizationID = string(org)
			cfg.Hosted.WorkOSOrganizationID = "provider_" + string(org)
			cfg.runMaintenance = func(ctx context.Context, scheduler *hubmaintenance.Scheduler, _ func(error)) error {
				started <- scheduler
				<-ctx.Done()
				return ctx.Err()
			}
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			}()
			service.workerCancel()
			<-service.workerDone
			var scheduler *hubmaintenance.Scheduler
			select {
			case scheduler = <-started:
			default:
				t.Fatal("hosted service never started read-model maintenance")
			}
			if len(started) != 0 {
				t.Fatal("multiple schedulers started")
			}
			for range 32 {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			for _, other := range tenants {
				if err := service.config.TenantRepository.ReadTenant(t.Context(), other, corestore.Serving, func(r tenantstore.Reader) error {
					ready, err := r.(viewstore.WorkItemPageReadinessReader).WorkItemPageReady(t.Context(), "example", "wi_example", "item")
					if err != nil {
						return err
					}
					want := other == org || org == "org_second"
					if ready != want {
						return fmt.Errorf("tenant %s ready=%v want=%v", other, ready, want)
					}
					if !ready {
						return nil
					}
					page, err := r.(viewstore.WorkItemPageReader).WorkItemDetailPage(t.Context(), "example", "wi_example")
					if err != nil {
						return err
					}
					if page.Value.OrganizationID != other || page.Value.Body != string(other) {
						return fmt.Errorf("page crossed tenant boundary: %+v", page.Value.NativeReference)
					}
					relationshipsReady, err := r.(viewstore.WorkItemPageReadinessReader).WorkItemPageReady(t.Context(), "example", "wi_example", "relationships")
					if err != nil {
						return err
					}
					if !relationshipsReady {
						return fmt.Errorf("tenant %s relationships pending", other)
					}
					board, err := r.BoardItem(t.Context(), "example", "wi_example")
					if err != nil {
						return err
					}
					if !board.Display.Ready || board.PageIssue == nil {
						return fmt.Errorf("tenant %s board pending", other)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestHostedMaintenanceCloseJoinsScheduler(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	cfg := startupConfig(connection)
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedStartupOrganization(t, store.postgres, "org_startup")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	started := make(chan struct{})
	exited := make(chan struct{})
	cfg.runMaintenance = func(ctx context.Context, scheduler *hubmaintenance.Scheduler, report func(error)) error {
		defer close(exited)
		close(started)
		return scheduler.Run(ctx, report)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned without joining scheduler")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	assertStartupDisconnected(t, admin, connection)
}
