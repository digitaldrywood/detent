package project

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/schedulehealth"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func TestHandleWorkflowUpdateKeepsScheduleHealthDefinitionsWhenPreparationFails(t *testing.T) {
	t.Parallel()

	initial := projectRaceWorkflowConfig()
	initial.Routines = []workflowconfig.Routine{{Name: "active", Schedule: "* * * * *", Prompt: "Run."}}
	initial.ScheduleOwnership = reloadScheduleOwnershipConfig()
	store := &reloadScheduleRunStore{}
	faults := make(chan error, 1)
	monitor, err := schedulehealth.New("detent", scheduleDefinitions(initial), store, schedulehealth.Dependencies{
		OnFault: func(err error, _ time.Time) {
			faults <- err
		},
	})
	if err != nil {
		t.Fatalf("schedulehealth.New() error = %v", err)
	}

	got := &Project{
		id:       "detent",
		cfg:      globalconfig.Project{ID: "detent"},
		workflow: workflowconfig.Workflow{Config: initial, Prompt: "initial"},
		connectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
			return nil, errors.New("connector unavailable")
		},
		scheduleConfig: initial.ScheduleOwnership.Normalized("", ""),
		scheduleHealth: monitor,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	reloaded := initial
	reloaded.Routines = []workflowconfig.Routine{{Name: "rejected", Schedule: "* * * * *", Prompt: "Run."}}
	err = got.handleWorkflowUpdate(t.Context(), configwatcher.Update{
		Workflow: workflowconfig.Workflow{Config: reloaded, Prompt: "rejected"},
	})
	if err == nil || !strings.Contains(err.Error(), "connector unavailable") {
		t.Fatalf("handleWorkflowUpdate() error = %v, want connector unavailable", err)
	}

	checkAt := time.Now().UTC().Add(3 * time.Minute)
	if err := store.RecordScheduledRun(t.Context(), schedulehealth.Run{
		ProjectID: "detent", ScheduleID: schedulehealth.RoutineID("active"), CompletedAt: checkAt.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("RecordScheduledRun() error = %v", err)
	}
	monitor.Check(t.Context(), checkAt)
	select {
	case fault := <-faults:
		t.Fatalf("schedule health fault after rejected reload = %v", fault)
	default:
	}
}

func reloadScheduleOwnershipConfig() scheduleowner.Config {
	return scheduleowner.Config{
		Enabled: true, Backend: scheduleowner.BackendGitHubRef, Key: "example/detent",
		Repository: "example/coordination", Branch: scheduleowner.DefaultBranch,
		LeaseSeconds: scheduleowner.DefaultLeaseSeconds, HeartbeatSeconds: scheduleowner.DefaultHeartbeatSeconds,
		RetrySeconds: scheduleowner.DefaultRetrySeconds, MaxClockSkewSeconds: scheduleowner.DefaultMaxClockSkewSeconds,
	}
}

type reloadScheduleRunStore struct {
	mu   sync.Mutex
	runs []schedulehealth.Run
}

func (s *reloadScheduleRunStore) LatestScheduledRun(_ context.Context, projectID string, scheduleID string) (schedulehealth.Run, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := len(s.runs) - 1; index >= 0; index-- {
		run := s.runs[index]
		if run.ProjectID == projectID && run.ScheduleID == scheduleID {
			return run, true, nil
		}
	}
	return schedulehealth.Run{}, false, nil
}

func (s *reloadScheduleRunStore) RecordScheduledRun(_ context.Context, run schedulehealth.Run) error {
	s.mu.Lock()
	s.runs = append(s.runs, run)
	s.mu.Unlock()
	return nil
}
