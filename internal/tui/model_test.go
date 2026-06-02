package tui

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestModelRendersSnapshotFromHub(t *testing.T) {
	t.Parallel()

	snapshots := hub.New[telemetry.Snapshot]()
	model, err := NewModel(context.Background(), snapshots, WithNow(func() time.Time {
		return time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	}))
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}

	cmd := model.Init()
	if err := snapshots.Publish(testSnapshot()); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	msg := cmd()
	next, nextCmd := model.Update(msg)
	if nextCmd == nil {
		t.Fatal("Update() did not return a follow-up subscription command")
	}

	view := stripANSI(next.(Model).View())
	for _, want := range []string{
		"DETENT STATUS",
		"Project: https://github.com/digitaldrywood/detent",
		"Instance: release-captain (detent-bot)",
		"Scope: assignee in @me (detent-bot, release-captain)",
		"Dashboard: http://localhost:4101",
		"Next refresh: 2026-05-31T00:16:00Z",
		"Agents: 1 running | 1 queued | 1 blocked | 1 completed",
		"Throughput: 7 tps",
		"Tokens: in 110 | out 220 | total 330",
		"Budget: enabled current $12.50 | projected $0.75 | day max $50.00 | issue max $5.00",
		"Rate Limits: codex-primary | primary 90/100 reset 60s | secondary 0/100 reset 30s | credits 0/1",
		"Running",
		"PID",
		"DD-44",
		"In Progress",
		"4242",
		"12m 5s / 3",
		"sess...567890",
		"turn completed",
		"Backoff queue",
		"attempt=2",
		"in 1.500s",
		"no available orchestrator slots",
		"Blocked",
		"dependency #9 is not merged",
		"Completed",
		"Done",
		"gpt-5",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "worker-1") {
		t.Fatalf("View() rendered worker host as process identity:\n%s", view)
	}
}

func TestModelRendersWaitingStateBeforeSnapshot(t *testing.T) {
	t.Parallel()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}

	view := stripANSI(model.View())
	for _, want := range []string{"DETENT STATUS", "Dashboard: http://localhost:4000", "Waiting for telemetry snapshot"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() missing %q:\n%s", want, view)
		}
	}
}

func TestModelHandlesWindowSize(t *testing.T) {
	t.Parallel()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}

	next, _ := model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	got := next.(Model)

	if got.width != 72 || got.height != 24 {
		t.Fatalf("window size = %dx%d, want 72x24", got.width, got.height)
	}
}

func TestModelClosesSubscriptionOnQuit(t *testing.T) {
	t.Parallel()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}

	if _, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); cmd == nil {
		t.Fatal("Update(q) did not return quit command")
	}

	select {
	case _, ok := <-model.subscription.C():
		if ok {
			t.Fatal("subscription channel is open after quit")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription channel to close")
	}
}

func TestNewModelRejectsNilHub(t *testing.T) {
	t.Parallel()

	if _, err := NewModel(context.Background(), nil); err == nil {
		t.Fatal("NewModel(nil) error = nil, want error")
	}
}

func testSnapshot() telemetry.Snapshot {
	generatedAt := time.Date(2026, 5, 31, 0, 15, 30, 0, time.UTC)
	startedAt := generatedAt.Add(-12*time.Minute - 5*time.Second)
	completedAt := generatedAt.Add(-2 * time.Minute)
	dayMax := 50.0
	issueMax := 5.0
	resetAt := generatedAt.Add(time.Minute)

	return telemetry.Snapshot{
		GeneratedAt: generatedAt,
		Project: telemetry.Project{
			DisplayName: "Detent",
			URL:         "https://github.com/digitaldrywood/detent",
		},
		Instance: telemetry.Instance{
			Name:                    "release-captain",
			GitHubLogin:             "detent-bot",
			AuthorizationScope:      "assignee in @me (detent-bot, release-captain)",
			AuthorizationConfigured: true,
		},
		DashboardURL: "http://localhost:4101",
		Refresh: telemetry.Refresh{
			PollIntervalSeconds: 30,
			LastRefreshAt:       &generatedAt,
			NextRefreshAt:       timePointer(generatedAt.Add(30 * time.Second)),
		},
		Counts: telemetry.Counts{
			Running:   1,
			Queue:     1,
			Blocked:   1,
			Completed: 1,
		},
		Running: []telemetry.Running{
			{
				Issue: telemetry.Issue{
					ID:         "I_kwDOSskuwc8AAAABD42jxg",
					Identifier: "DD-44",
					State:      "In Progress",
					Title:      "feat(tui): bubbletea ANSI dashboard on hub",
				},
				WorkerHost:      "worker-1",
				ProcessIdentity: "4242",
				WorkspacePath:   "/tmp/detent/worktree",
				SessionID:       "session-1234567890",
				TurnCount:       3,
				StartedAt:       startedAt,
				LastEvent:       "turn_completed",
				LastMessage:     "turn completed",
				RuntimeSeconds:  12*60 + 5,
				Tokens: telemetry.Tokens{
					Input:  40,
					Output: 60,
					Total:  100,
				},
			},
		},
		Queue: []telemetry.Queued{
			{
				Issue: telemetry.Issue{
					ID:         "queue-1",
					Identifier: "DD-45",
					State:      "Todo",
					Title:      "queued work",
				},
				Attempt:     2,
				DueInMillis: 1500,
				Error:       "no available orchestrator slots",
			},
		},
		Blocked: []telemetry.Blocked{
			{
				Issue: telemetry.Issue{
					ID:         "blocked-1",
					Identifier: "DD-46",
					State:      "Blocked",
					Title:      "blocked work",
				},
				Error: "dependency #9 is not merged",
			},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:         "completed-1",
					Identifier: "DD-47",
					State:      "Done",
					Title:      "completed work",
				},
				StartedAt:      startedAt,
				CompletedAt:    completedAt,
				Turns:          4,
				RuntimeSeconds: 605,
				FinalState:     "Done",
				Model:          "gpt-5",
				Tokens: telemetry.Tokens{
					Input:  10,
					Output: 20,
					Total:  30,
				},
			},
		},
		Budget: telemetry.Budget{
			Enabled:          true,
			PerDayMaxUSD:     &dayMax,
			PerIssueMaxUSD:   &issueMax,
			CurrentSpendUSD:  12.5,
			ProjectedCostUSD: 0.75,
		},
		RateLimits: &telemetry.RateLimits{
			LimitID: "codex-primary",
			Primary: &telemetry.RateLimitBucket{
				Remaining:      90,
				Limit:          100,
				ResetAt:        &resetAt,
				ResetInSeconds: 60,
			},
			Secondary: &telemetry.RateLimitBucket{
				Limit:          100,
				ResetInSeconds: 30,
			},
			Credits: &telemetry.RateLimitBucket{
				Limit: 1,
			},
		},
		Tokens: telemetry.Tokens{
			Input:          110,
			Output:         220,
			Total:          330,
			RuntimeSeconds: 725,
		},
		Throughput: telemetry.TokenThroughput{
			TokensPerSecond: 7,
			WindowSeconds:   60,
			Tokens:          420,
		},
	}
}

func stripANSI(value string) string {
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	return ansi.ReplaceAllString(value, "")
}

func timePointer(value time.Time) *time.Time {
	return &value
}
