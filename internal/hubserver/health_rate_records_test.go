package hubserver

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthAttemptReadCounts struct{ singles, batches int }

type healthAttemptCountingRepository struct {
	tenantstore.Repository
	snapshot tenantstore.SnapshotRepository
	counts   *healthAttemptReadCounts
}

func (r healthAttemptCountingRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.snapshot.ReadTenantSnapshot(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(healthAttemptCountingReader{Reader: reader, attemptArchiveReader: reader.(attemptArchiveReader), ObservationReader: reader.(healthstore.ObservationReader), AttemptSourceReader: reader.(healthstore.AttemptSourceReader), HistoryReader: reader.(reportstore.HistoryReader), AttemptReader: reader.(reportstore.AttemptReader), LandingReader: reader.(reportstore.LandingReader), counts: r.counts})
	})
}

type healthAttemptCountingReader struct {
	tenantstore.Reader
	attemptArchiveReader
	healthstore.ObservationReader
	healthstore.AttemptSourceReader
	reportstore.HistoryReader
	reportstore.AttemptReader
	reportstore.LandingReader
	counts *healthAttemptReadCounts
}

func (r healthAttemptCountingReader) Attempt(ctx context.Context, id string) (leasestore.Attempt, error) {
	r.counts.singles++
	return r.Reader.Attempt(ctx, id)
}

func (r healthAttemptCountingReader) HealthAttemptSources(ctx context.Context, project tracker.ProjectID, ids []string) (healthstore.AttemptSources, error) {
	r.counts.batches++
	return r.AttemptSourceReader.HealthAttemptSources(ctx, project, ids)
}

func TestHealthRateSnapshotUsesCurrentAttemptPayload(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	original := service.config.TenantRepository
	t.Cleanup(func() { service.config.TenantRepository = original })
	now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service.config.TenantRepository = original
		f := newNativeFixture(t, service, org, "rate-history")
		expected := map[string]string{}
		for _, title := range []string{"first", "second"} {
			issue := f.create(t, title)
			canonical := tracker.NativeRunData{SessionID: "current-" + title, Finalization: &tracker.NativeFinalization{Settled: true, Changed: true, VersionID: "current-version-" + title}, Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: now.Add(-time.Minute)}}
			id := seedHealthAttemptRecord(t, f, issue, "rate-machine-"+title, "succeeded", canonical, now.Add(-time.Minute), -1)
			var attempt leasestore.Attempt
			if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				attempt, err = r.Attempt(t.Context(), id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			stale := tracker.NativeRunData{AttemptID: id, RunID: attempt.RunID, LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken), Sequence: attempt.Sequence, Outcome: attempt.Status, SessionID: "stale-" + title, Finalization: &tracker.NativeFinalization{Settled: true, Changed: true, VersionID: "stale-version-" + title}, Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: now.Add(-time.Hour)}}
			appendTestHealthEvent(t, f, issue, "run.finished", tracker.CollaborationData{Run: &stale}, now.Add(-time.Second))
			expected[id] = title
		}
		counts := &healthAttemptReadCounts{}
		service.config.TenantRepository = healthAttemptCountingRepository{Repository: original, snapshot: original.(tenantstore.SnapshotRepository), counts: counts}
		snapshot := healthSnapshot{Projects: []healthProject{{ID: string(f.project.ID)}}}
		if err := service.readHealthRateRecords(t.Context(), org, now, nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Rates) != 1 || len(snapshot.Rates[0].Attempts) != 2 {
			t.Fatalf("organization=%s rates=%+v", org, snapshot.Rates)
		}
		if counts.singles != 0 || counts.batches != 1 {
			t.Fatalf("organization=%s attempt source reads=%+v", org, counts)
		}
		for _, attempt := range snapshot.Rates[0].Attempts {
			title, ok := expected[attempt.ID]
			if !ok || attempt.Session != "current-"+title || attempt.SourceVersion != "current-version-"+title || !attempt.Heartbeat.Equal(now.Add(-time.Minute)) || !attempt.At.Equal(now.Add(-time.Minute)) {
				t.Fatalf("organization=%s canonical attempt lost: %+v", org, attempt)
			}
		}
		ids := slices.Sorted(maps.Keys(expected))
		requested := append(slices.Repeat([]string{ids[0]}, corestore.MaxPageSize), ids[1])
		if err := service.config.TenantRepository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			cached, err := readHealthAttemptSources(t.Context(), r, org, f.project.ID, requested)
			if err != nil {
				return err
			}
			for _, id := range ids {
				attempt, err := cached.Attempt(t.Context(), id)
				if err != nil {
					return err
				}
				lease, err := cached.Lease(t.Context(), attempt.LeaseID)
				if err != nil {
					return err
				}
				event, err := cached.LatestRunEvent(t.Context(), f.project.ID, id)
				if err != nil {
					return err
				}
				object, err := cached.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), attempt.PayloadObjectKey)
				if err != nil {
					return err
				}
				if lease.OrganizationID != org || lease.FencingToken != attempt.FencingToken || event.AttemptID != id || object.OrganizationID != org {
					t.Fatal("health source chunk merge lost identity")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if counts.singles != 0 || counts.batches != 3 {
			t.Fatalf("organization=%s chunked attempt source reads=%+v", org, counts)
		}
	}
}
