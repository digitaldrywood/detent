package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

func TestCheckPauseExitConditionsAutoUnpausesEligibleProjects(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := globalconfig.Config{
		Projects: []globalconfig.Project{
			{
				ID:               "issue-pause",
				Paused:           true,
				PausedReason:     "wait for fix",
				PausedAt:         now.Add(-24 * time.Hour).Format(time.RFC3339),
				PausedUntilIssue: "digitaldrywood/detent#1499",
			},
			{
				ID:           "time-pause",
				Paused:       true,
				PausedReason: "maintenance",
				PausedAt:     now.Add(-time.Hour).Format(time.RFC3339),
				PausedUntil:  now.Add(-time.Minute).Format(time.RFC3339),
			},
			{
				ID:       "legacy-pause",
				Paused:   true,
				PausedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			},
		},
	}
	projectConnector := memory.New(memory.Config{Issues: []connector.Issue{{
		Identifier: "digitaldrywood/detent#1499",
		Closed:     true,
	}}})

	current := clonePauseMonitorConfig(cfg)
	var writes int
	var unpaused []string
	var connectorProjects []string
	var logs bytes.Buffer
	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			return clonePauseMonitorConfig(current), nil
		},
		write: func(updated globalconfig.Config) error {
			writes++
			current = clonePauseMonitorConfig(updated)
			return nil
		},
		unpause: func(_ context.Context, projectID string) error {
			unpaused = append(unpaused, projectID)
			return nil
		},
		connectorFor: func(projectID string) connector.Connector {
			connectorProjects = append(connectorProjects, projectID)
			return projectConnector
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}
	got := current.Projects
	for _, index := range []int{0, 1} {
		if got[index].Paused ||
			got[index].PausedReason != "" ||
			got[index].PausedAt != "" ||
			got[index].PausedUntilIssue != "" ||
			got[index].PausedUntil != "" {
			t.Fatalf("project %d pause metadata = %#v, want cleared", index, got[index])
		}
	}
	if !got[2].Paused {
		t.Fatal("legacy bare pause was lifted")
	}
	if !reflect.DeepEqual(connectorProjects, []string{"issue-pause"}) {
		t.Fatalf("connector projects = %#v, want only issue-pause", connectorProjects)
	}
	if !reflect.DeepEqual(unpaused, []string{"issue-pause", "time-pause"}) {
		t.Fatalf("unpaused projects = %#v, want issue and time pauses", unpaused)
	}
	if count := strings.Count(logs.String(), "project automatically unpaused"); count != 2 {
		t.Fatalf("automatic unpause log count = %d, want 2:\n%s", count, logs.String())
	}
}

func TestCheckPauseExitConditionsLeavesOpenIssuePaused(t *testing.T) {
	t.Parallel()

	cfg := globalconfig.Config{Projects: []globalconfig.Project{{
		ID:               "issue-pause",
		Paused:           true,
		PausedUntilIssue: "digitaldrywood/detent#1499",
	}}}
	projectConnector := memory.New(memory.Config{Issues: []connector.Issue{{
		Identifier: "digitaldrywood/detent#1499",
	}}})
	wrote := false

	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			return cfg, nil
		},
		write: func(globalconfig.Config) error {
			wrote = true
			return nil
		},
		connectorFor: func(string) connector.Connector {
			return projectConnector
		},
		now: func() time.Time {
			return time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
		},
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if wrote {
		t.Fatal("pause monitor wrote config for an open exit issue")
	}
}

func TestCheckPauseExitConditionsPreservesConcurrentConfigEdit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	evaluated := globalconfig.Config{Projects: []globalconfig.Project{{
		ID:          "time-pause",
		Paused:      true,
		PausedUntil: now.Add(-time.Minute).Format(time.RFC3339),
		Priority:    1,
	}}}
	latest := clonePauseMonitorConfig(evaluated)
	latest.Projects[0].Priority = 9
	reads := 0
	var written globalconfig.Config

	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			reads++
			if reads == 1 {
				return clonePauseMonitorConfig(evaluated), nil
			}
			return clonePauseMonitorConfig(latest), nil
		},
		write: func(updated globalconfig.Config) error {
			written = clonePauseMonitorConfig(updated)
			return nil
		},
		unpause: func(context.Context, string) error {
			return nil
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if len(written.Projects) != 1 {
		t.Fatalf("written projects = %d, want 1", len(written.Projects))
	}
	if written.Projects[0].Priority != 9 {
		t.Fatalf("written priority = %d, want concurrent value 9", written.Projects[0].Priority)
	}
	if written.Projects[0].Paused {
		t.Fatal("eligible project remained paused")
	}
}

func TestCheckPauseExitConditionsSkipsChangedPause(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	evaluated := globalconfig.Config{Projects: []globalconfig.Project{{
		ID:          "time-pause",
		Paused:      true,
		PausedUntil: now.Add(-time.Minute).Format(time.RFC3339),
	}}}
	latest := clonePauseMonitorConfig(evaluated)
	latest.Projects[0].PausedUntil = now.Add(time.Hour).Format(time.RFC3339)
	reads := 0
	unpaused := false
	wrote := false

	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			reads++
			if reads == 1 {
				return clonePauseMonitorConfig(evaluated), nil
			}
			return clonePauseMonitorConfig(latest), nil
		},
		write: func(globalconfig.Config) error {
			wrote = true
			return nil
		},
		unpause: func(context.Context, string) error {
			unpaused = true
			return nil
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if unpaused {
		t.Fatal("runtime unpaused after the exit condition changed")
	}
	if wrote {
		t.Fatal("config written after the exit condition changed")
	}
}

func TestCheckPauseExitConditionsRequiresRuntimeAcceptance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{
		ID:          "time-pause",
		Paused:      true,
		PausedUntil: now.Add(-time.Minute).Format(time.RFC3339),
	}}}
	wrote := false

	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			return clonePauseMonitorConfig(cfg), nil
		},
		write: func(globalconfig.Config) error {
			wrote = true
			return nil
		},
		unpause: func(context.Context, string) error {
			return errors.New("workflow is invalid")
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if wrote {
		t.Fatal("config written after runtime rejected the unpause")
	}
}

func TestCheckPauseExitConditionsRollsBackRuntimeAfterWriteFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{
		ID:          "time-pause",
		Paused:      true,
		PausedUntil: now.Add(-time.Minute).Format(time.RFC3339),
	}}}
	unpauseCalls := 0
	pauseCalls := 0

	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			return clonePauseMonitorConfig(cfg), nil
		},
		write: func(globalconfig.Config) error {
			return errors.New("disk full")
		},
		unpause: func(context.Context, string) error {
			unpauseCalls++
			return nil
		},
		pause: func(context.Context, string) error {
			pauseCalls++
			return nil
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if unpauseCalls != 1 {
		t.Fatalf("runtime unpause calls = %d, want 1", unpauseCalls)
	}
	if pauseCalls != 1 {
		t.Fatalf("runtime pause rollback calls = %d, want 1", pauseCalls)
	}
}

func clonePauseMonitorConfig(cfg globalconfig.Config) globalconfig.Config {
	cloned := cfg
	cloned.Projects = append([]globalconfig.Project(nil), cfg.Projects...)
	return cloned
}
