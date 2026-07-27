package cli

import (
	"bytes"
	"context"
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

	var written []globalconfig.Config
	var connectorProjects []string
	var logs bytes.Buffer
	checkPauseExitConditions(context.Background(), pauseMonitorDeps{
		read: func() (globalconfig.Config, error) {
			return cfg, nil
		},
		write: func(updated globalconfig.Config) error {
			written = append(written, updated)
			return nil
		},
		connectorFor: func(projectID string) connector.Connector {
			connectorProjects = append(connectorProjects, projectID)
			return projectConnector
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})

	if len(written) != 1 {
		t.Fatalf("writes = %d, want 1", len(written))
	}
	got := written[0].Projects
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
