package hubserver

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthAdmissionReadCounts struct{ single, batches, projectSingles, projectBatches int }

type healthAdmissionCountingRepository struct {
	tenantstore.Repository
	snapshot tenantstore.SnapshotRepository
	counts   *healthAdmissionReadCounts
}

func (r healthAdmissionCountingRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.snapshot.ReadTenantSnapshot(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return r.read(reader, read)
	})
}

func (r healthAdmissionCountingRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { return r.read(reader, read) })
}

func (r healthAdmissionCountingRepository) read(reader tenantstore.Reader, read func(tenantstore.Reader) error) error {
	observations, ok := reader.(healthstore.ObservationReader)
	if !ok {
		return workItemStorageUnavailable()
	}
	admissions, ok := reader.(runnerstore.AdmissionReader)
	if !ok {
		return workItemStorageUnavailable()
	}
	return read(healthAdmissionCountingReader{Reader: reader, ObservationReader: observations, AdmissionReader: admissions, counts: r.counts})
}

type healthAdmissionCountingReader struct {
	tenantstore.Reader
	healthstore.ObservationReader
	runnerstore.AdmissionReader
	counts *healthAdmissionReadCounts
}

func (r healthAdmissionCountingReader) RunnerAdmissionObservation(ctx context.Context, runner string, project tracker.ProjectID) (tracker.NativeAdmissionObservation, error) {
	r.counts.single++
	return r.Reader.RunnerAdmissionObservation(ctx, runner, project)
}

func (r healthAdmissionCountingReader) RunnerAdmissionObservations(ctx context.Context, project tracker.ProjectID, runners []string) (map[string]tracker.NativeAdmissionObservation, error) {
	r.counts.batches++
	return r.AdmissionReader.RunnerAdmissionObservations(ctx, project, runners)
}

func (r healthAdmissionCountingReader) RunnerProjects(ctx context.Context, runner string, limit int) ([]tracker.ProjectID, error) {
	r.counts.projectSingles++
	return r.Reader.RunnerProjects(ctx, runner, limit)
}

func (r healthAdmissionCountingReader) HealthRunnerProjects(ctx context.Context, runners []string, limit int) (map[string][]tracker.ProjectID, error) {
	r.counts.projectBatches++
	return r.ObservationReader.HealthRunnerProjects(ctx, runners, limit)
}

func TestPostgresHealthAdmissionsUseProjectBatches(t *testing.T) {
	service := newPostgresNativeService(t, Config{})
	original := service.config.TenantRepository
	t.Cleanup(func() { service.config.TenantRepository = original })
	allRunners := map[tracker.OrganizationID][]string{}
	for _, test := range []struct {
		org              tracker.OrganizationID
		runners, batches int
	}{{"org_first", 101, 2}, {"org_first_second", 2, 1}} {
		service.config.TenantRepository = original
		f := newNativeFixture(t, service, test.org, "Example")
		expected := map[string]tracker.NativeAdmissionObservation{}
		for i := range test.runners {
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			now := service.config.now().UTC().Truncate(time.Microsecond)
			observation := tracker.NativeAdmissionObservation{RunnerRevision: 1, ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: hubTestPolicy().ID, LastRefreshAt: now, RefreshDuration: time.Duration(i+1) * time.Second}}
			if err := original.WriteTenant(t.Context(), test.org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerAdmissionObservation(t.Context(), runner.binding.RunnerID, f.project.ID, observation)
			}); err != nil {
				t.Fatal(err)
			}
			expected[runner.binding.RunnerID] = observation
			allRunners[test.org] = append(allRunners[test.org], runner.binding.RunnerID)
		}
		counts := &healthAdmissionReadCounts{}
		service.config.TenantRepository = healthAdmissionCountingRepository{Repository: original, snapshot: original.(tenantstore.SnapshotRepository), counts: counts}
		runners, err := service.readHealthRunnerRecords(t.Context(), test.org, service.config.now())
		if err != nil {
			t.Fatal(err)
		}
		if len(runners) != test.runners || counts.single != 0 || counts.batches != test.batches || counts.projectSingles != 0 || counts.projectBatches != 1 {
			t.Fatalf("organization=%s runners=%d reads=%+v", test.org, len(runners), counts)
		}
		for _, runner := range runners {
			want, ok := expected[runner.ID]
			got, found := runner.Admissions[string(f.project.ID)]
			if !ok || !found || len(runner.Projects) != 1 || runner.Projects[0] != string(f.project.ID) || got.Context.RefreshDuration != want.Context.RefreshDuration || !got.Context.LastRefreshAt.Equal(want.Context.LastRefreshAt) || got.RunnerRevision != want.RunnerRevision {
				t.Fatalf("runner=%s admission=%+v want=%+v", runner.ID, got, want)
			}
		}
	}
	if err := original.ReadTenant(t.Context(), "org_first", corestore.Background, func(r tenantstore.Reader) error {
		projects, err := r.(healthstore.ObservationReader).HealthRunnerProjects(t.Context(), allRunners["org_first_second"], healthProjectLimit+1)
		if err == nil && len(projects) != 0 {
			t.Fatalf("foreign runner projects exposed: %+v", projects)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
