package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func testHostSummary(now time.Time) hostmetrics.Summary {
	return hostmetrics.Summary{
		Hour: now.UTC().Truncate(time.Hour), SegmentID: now.Add(-time.Minute), Partial: true, SampleCount: 2, LogicalCores: 8,
		MemoryTotalBytes: 1000, MemoryAvailableMinBytes: 100, MemoryAvailableSumBytes: 400, MemorySampleCount: 2,
		SwapUsedMaxBytes: 50, SwapSampleCount: 2, PSISomeAvg10Sum: 20, PSISomeSampleCount: 2, PSIFullAvg10Sum: 4, PSIFullSampleCount: 2,
		PressureWarnCount: 1, PressureSampleCount: 2, CPUBusySumPercent: 80, CPUBusyMaxPercent: 60, CPUSampleCount: 2,
		Load1Max: 3, LoadSampleCount: 2, DiskFreeMinBytes: 100, DiskTotalMinBytes: 1000, DiskSampleCount: 2,
	}
}

func TestRunnerHostSummaries(t *testing.T) {
	now := time.Now().UTC()
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
	r.enroll(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "active runner host summary")
	claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "metrics", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	claimResponse := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, claimResponse, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimResponse, &lease)
	var expiresBefore string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT expires_at FROM leases WHERE lease_id=? AND released_at IS NULL", lease.ID).Scan(&expiresBefore); err != nil {
		t.Fatal(err)
	}
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	send := func(raw any) runnerauth.RoutingSnapshot {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "host_metrics": raw})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		return snapshot
	}
	a := testHostSummary(now)
	b := a
	b.SegmentID = now
	b.MemoryAvailableMinBytes, b.MemoryAvailableSumBytes = 200, 800
	b.SwapUsedMaxBytes, b.CPUBusySumPercent, b.CPUBusyMaxPercent = 80, 100, 70
	b.Load1Max, b.DiskFreeMinBytes, b.DiskTotalMinBytes = 5, 50, 900
	for _, summaries := range [][]hostmetrics.Summary{{a}, {b}, {a, b}} {
		if snapshot := send(summaries); len(snapshot.HostMetricsAcknowledged) != len(summaries) {
			t.Fatalf("missing acknowledgments: %+v", snapshot)
		}
	}
	var expiresAfter string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT expires_at FROM leases WHERE lease_id=? AND released_at IS NULL", lease.ID).Scan(&expiresAfter); err != nil {
		t.Fatal(err)
	}
	runner, err := readRunnerWithClock(t.Context(), f.service.database.reader, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now)
	if err != nil {
		t.Fatal(err)
	}
	if expiresAfter != expiresBefore || runner.ReportedCapacity != 2 || !runner.LastHeartbeatAt.Equal(now) {
		t.Fatalf("shutdown partial changed runner availability or lease: %+v expiry=%s", runner, expiresAfter)
	}
	history, err := f.service.readRunnerHostHistory(t.Context(), f.project.OrganizationID, r.binding.RunnerID, a.Hour, a.Hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("hours=%d", len(history))
	}
	got := history[0]
	if got.SampleCount != 4 || got.MemoryAvailableMinBytes != 100 || got.MemoryAvailableSumBytes != 1200 || got.MemorySampleCount != 4 || *got.MemoryAvailableAverageBytes != 300 || got.SwapUsedMaxBytes != 80 || got.CPUBusyMaxPercent != 70 || *got.CPUBusyAveragePercent != 45 || got.Load1Max != 5 || got.DiskFreeMinBytes != 50 || got.DiskTotalMinBytes != 900 || got.PressureWarnCount != 2 || *got.PSISomeAvg10Average != 10 || *got.PSIFullAvg10Average != 2 {
		t.Fatalf("incorrect merged hour: %+v", got)
	}
	for _, test := range []struct {
		name string
		raw  any
	}{
		{"malformed field", map[string]any{"hour": "bad"}},
		{"malformed segment", []any{map[string]any{"sample_count": "bad"}}},
		{"oversized segment", []any{map[string]any{"ignored": strings.Repeat("x", 1024)}}},
		{"too many segments", make([]hostmetrics.Summary, 25)},
		{"negative metric", []any{map[string]any{"cpu_busy_sum_percent": -1}}},
		{"future hour", []hostmetrics.Summary{func() hostmetrics.Summary { s := a; s.Hour = a.Hour.Add(time.Hour); return s }()}},
		{"counts exceed samples", []hostmetrics.Summary{func() hostmetrics.Summary { s := a; s.MemorySampleCount = 3; return s }()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if snapshot := send(test.raw); len(snapshot.HostMetricsAcknowledged) != 0 {
				t.Fatalf("invalid summary acknowledged: %+v", snapshot)
			}
		})
	}
	var stored string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT summary_json FROM runner_host_hours WHERE runner_id=?", r.binding.RunnerID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var unchanged hostmetrics.Summary
	if err := json.Unmarshal([]byte(stored), &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.SampleCount != 4 {
		t.Fatalf("bad summaries changed count: %d", unchanged.SampleCount)
	}
	other, err := f.service.readRunnerHostHistory(t.Context(), tracker.OrganizationID("org_other"), r.binding.RunnerID, a.Hour, a.Hour.Add(time.Hour))
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-organization history: %v %v", other, err)
	}
	for _, bounds := range [][2]time.Time{{a.Hour, a.Hour}, {a.Hour, a.Hour.Add(721 * time.Hour)}} {
		if _, err := f.service.readRunnerHostHistory(t.Context(), f.project.OrganizationID, r.binding.RunnerID, bounds[0], bounds[1]); err == nil {
			t.Fatal("unbounded range accepted")
		}
	}
	for index := range maxHostSegments + 1 {
		segment := a
		segment.SegmentID = now.Add(-time.Duration(index+2) * time.Second)
		send([]hostmetrics.Summary{segment})
	}
	var ids string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT segment_ids_json FROM runner_host_hours WHERE runner_id=?", r.binding.RunnerID).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	var segments []time.Time
	if err := json.Unmarshal([]byte(ids), &segments); err != nil {
		t.Fatal(err)
	}
	if len(segments) != maxHostSegments {
		t.Fatalf("segment IDs grew to %d", len(segments))
	}
	history, err = f.service.readRunnerHostHistory(t.Context(), f.project.OrganizationID, r.binding.RunnerID, a.Hour, a.Hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].SampleCount != 256 {
		t.Fatalf("segments beyond bound changed aggregate: %+v", history)
	}
}
