package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestCheckDoctorOrphanedAgentProcesses(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		summary    telemetry.OrphanedAgentProcesses
		wantStatus doctorStatus
		wantDetail []string
	}{
		{
			name:       "healthy",
			wantStatus: doctorOK,
			wantDetail: []string{"no agent processes"},
		},
		{
			name: "orphan reports age and memory",
			summary: telemetry.OrphanedAgentProcesses{
				Count:         2,
				SessionCount:  1,
				TotalRSSBytes: 512 * 1024 * 1024,
				Processes: []telemetry.OrphanedAgentProcess{{
					SessionID:  1885,
					Identifier: "digitaldrywood/detent#1885",
					PID:        4242,
					StartedAt:  startedAt,
					AgeSeconds: 2 * 60 * 60,
					RSSBytes:   512 * 1024 * 1024,
				}},
			},
			wantStatus: doctorWarn,
			wantDetail: []string{"2 orphaned", "digitaldrywood/detent#1885", "pid=4242", "age=2h0m0s", "rss=512.0 MiB"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := checkDoctorOrphanedAgentProcesses(tt.summary)
			if check.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", check.Status, tt.wantStatus)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("detail = %q, want %q", check.Detail, want)
				}
			}
			if tt.summary.Count > 0 && !strings.Contains(check.Hint, "fix worker-processes --yes") {
				t.Fatalf("hint = %q, want reap command", check.Hint)
			}
		})
	}
}

func TestApplyWorkerProcessesFixRecordsReapCause(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	reapedAt := startedAt.Add(time.Hour)
	processStore := &workerProcessesFixTestStore{}
	result := workerProcessesFixResult{Processes: []workerProcessFixOutcome{{SessionID: 1885, PID: 4242, GroupID: 4242, StartedAt: startedAt}}}
	var gotIdentity procgroup.Identity
	err := applyWorkerProcessesFix(
		context.Background(),
		processStore,
		&result,
		func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
			gotIdentity = identity
			return procgroup.TerminationOutcomeKilled, nil
		},
		func() time.Time { return reapedAt },
	)
	if err != nil {
		t.Fatalf("applyWorkerProcessesFix() error = %v", err)
	}
	if gotIdentity != (procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: startedAt}) {
		t.Fatalf("reaped identity = %#v", gotIdentity)
	}
	if len(processStore.reaps) != 1 || processStore.reaps[0].Reason != "doctor_reap" || processStore.reaps[0].Outcome != store.WorkerProcessOutcomeKilled || !processStore.reaps[0].ReapedAt.Equal(reapedAt) {
		t.Fatalf("reap records = %#v", processStore.reaps)
	}
}

type workerProcessesFixTestStore struct {
	reaps []store.WorkerProcessReap
}

func (s *workerProcessesFixTestStore) MarkSessionWorkerProcessReaped(_ context.Context, _ int64, reap store.WorkerProcessReap) error {
	s.reaps = append(s.reaps, reap)
	return nil
}

func (*workerProcessesFixTestStore) Close() error {
	return nil
}
