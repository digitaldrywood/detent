package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantServesRunnersWhileReadsAreSlow(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "tenant-load")
	items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	worker := f.worker(t, "worker")
	f.service.tokenUseWork.Wait()
	ctx := testenv.WaitContext(t, t.Context())
	repository := f.service.database.postgres
	serving, background := repository.WorkloadCapacities()
	if background != 1 {
		t.Fatalf("background capacity=%d want=1", background)
	}
	writerReady, releaseWriter := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	rollback := errors.New("release held project write")
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseWriter)
			if err := <-writerDone; !errors.Is(err, rollback) {
				t.Errorf("held project write: %v", err)
			}
		})
	}
	defer release()
	go func() {
		writerDone <- repository.WriteTenant(ctx, f.project.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(ctx, f.project.ID)
			if err != nil {
				return err
			}
			if _, err := w.UpdateProjectRecord(ctx, record); err != nil {
				return err
			}
			close(writerReady)
			select {
			case <-releaseWriter:
			case <-ctx.Done():
			}
			return rollback
		})
	}()
	select {
	case <-writerReady:
	case err := <-writerDone:
		writerDone <- err
		t.Fatalf("project write exited before readiness: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseReaders := holdTenantLoadReads(t, repository, f.project.OrganizationID, corestore.Serving, serving)
	defer releaseReaders()
	if got := repository.Stats().AcquiredConns(); int(got) != serving+background {
		t.Fatalf("occupied PostgreSQL connections=%d want=%d", got, serving+background)
	}
	for _, token := range []string{f.token, worker} {
		credential, status, err := f.service.authenticateAPIToken(ctx, token, "", "")
		if err != nil || status != http.StatusOK || credential.ID == "" {
			t.Fatalf("authentication with occupied reader and writer pools: status=%d credential=%q error=%v", status, credential.ID, err)
		}
	}
	if credential, status, err := f.service.authenticateAPIToken(ctx, f.token+"-invalid", "", ""); status != http.StatusUnauthorized || err == nil || credential.ID != "" {
		t.Fatalf("invalid credential with occupied workloads: status=%d credential=%q error=%v", status, credential.ID, err)
	}
	if got := repository.Stats().AcquiredConns(); int(got) != serving+background {
		t.Fatalf("authentication released held workloads: acquired=%d want=%d", got, serving+background)
	}
	releaseReaders()
	for _, item := range items {
		for _, suffix := range []string{"comments", "runtime"} {
			request := newHubAPIRequest(t, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/"+suffix, f.token, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
}

func holdTenantLoadReads(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID, workload corestore.Workload, count int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	release, done := make(chan struct{}), make(chan error, count)
	var once sync.Once
	held := 0
	finish := func() {
		once.Do(func() {
			close(release)
			for range held {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
			cancel()
		})
	}
	t.Cleanup(finish)
	for range count {
		ready := make(chan struct{})
		held++
		go func() {
			done <- repository.ReadTenant(ctx, organization, workload, func(r tenantstore.Reader) error {
				if _, err := r.AccessOrganization(ctx); err != nil {
					return err
				}
				close(ready)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-ready:
		case err := <-done:
			held--
			t.Fatalf("held PostgreSQL reader exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	return finish
}

func warmReaderPools(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID) {
	t.Helper()
	serving, background := repository.WorkloadCapacities()
	releaseServing := holdTenantLoadReads(t, repository, organization, corestore.Serving, serving)
	defer releaseServing()
	releaseBackground := holdTenantLoadReads(t, repository, organization, corestore.Background, background)
	defer releaseBackground()
	if err := repository.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := repository.Stats()
	if stats.TotalConns() != stats.MaxConns() || int(stats.AcquiredConns()) != serving+background {
		t.Fatalf("PostgreSQL pool was not fully warmed: total=%d max=%d acquired=%d workloads=%d", stats.TotalConns(), stats.MaxConns(), stats.AcquiredConns(), serving+background)
	}
}
