package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthRateSnapshotUsesCurrentAttemptPayload(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
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
		snapshot := healthSnapshot{Projects: []healthProject{{ID: string(f.project.ID)}}}
		if err := service.readHealthRateRecords(t.Context(), org, now, nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Rates) != 1 || len(snapshot.Rates[0].Attempts) != 2 {
			t.Fatalf("organization=%s rates=%+v", org, snapshot.Rates)
		}
		for _, attempt := range snapshot.Rates[0].Attempts {
			title, ok := expected[attempt.ID]
			if !ok || attempt.Session != "current-"+title || attempt.SourceVersion != "current-version-"+title || !attempt.Heartbeat.Equal(now.Add(-time.Minute)) || !attempt.At.Equal(now.Add(-time.Minute)) {
				t.Fatalf("organization=%s canonical attempt lost: %+v", org, attempt)
			}
		}
	}
}
