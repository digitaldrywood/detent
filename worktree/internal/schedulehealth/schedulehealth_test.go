package schedulehealth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMonitorCheckReportsMissedIntervalsAndRecovers(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		definition Definition
		checkAt    time.Time
	}{
		{name: "routine", definition: Definition{ID: RoutineID("audit"), Schedule: "* * * * *"}, checkAt: start.Add(2 * time.Minute)},
		{name: "admission", definition: Definition{ID: AdmissionID, Schedule: "*/5 * * * *"}, checkAt: start.Add(10 * time.Minute)},
		{name: "intake", definition: Definition{ID: IntakeID("todos"), Schedule: "*/10 * * * *"}, checkAt: start.Add(20 * time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &memoryStore{}
			var mu sync.Mutex
			var fault error
			healthy := false
			monitor, err := New("detent", []Definition{tt.definition}, store, Dependencies{
				Now: func() time.Time { return start },
				OnFault: func(err error, _ time.Time) {
					mu.Lock()
					fault = err
					mu.Unlock()
				},
				OnHealthy: func() {
					mu.Lock()
					healthy = true
					fault = nil
					mu.Unlock()
				},
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			monitor.Check(t.Context(), tt.checkAt)
			mu.Lock()
			if fault == nil || !strings.Contains(fault.Error(), tt.definition.ID) || !strings.Contains(fault.Error(), "2 expected intervals") {
				t.Fatalf("fault = %v, want missed interval fault for %s", fault, tt.definition.ID)
			}
			mu.Unlock()

			runAt := tt.checkAt.Add(time.Second)
			if err := monitor.RecordScheduledRun(t.Context(), Run{
				ScheduleID: tt.definition.ID, ScheduledFor: runAt, StartedAt: runAt, CompletedAt: runAt,
			}); err != nil {
				t.Fatalf("RecordScheduledRun() error = %v", err)
			}
			monitor.Check(t.Context(), runAt)
			mu.Lock()
			defer mu.Unlock()
			if fault != nil || !healthy {
				t.Fatalf("fault = %v, healthy = %t, want recovered", fault, healthy)
			}
		})
	}
}

type memoryStore struct {
	mu   sync.Mutex
	runs []Run
}

func (s *memoryStore) LatestScheduledRun(_ context.Context, projectID string, scheduleID string) (Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := len(s.runs) - 1; index >= 0; index-- {
		run := s.runs[index]
		if run.ProjectID == projectID && run.ScheduleID == scheduleID {
			return run, true, nil
		}
	}
	return Run{}, false, nil
}

func (s *memoryStore) RecordScheduledRun(_ context.Context, run Run) error {
	s.mu.Lock()
	s.runs = append(s.runs, run)
	s.mu.Unlock()
	return nil
}
