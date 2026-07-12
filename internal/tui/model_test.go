package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestModelViewGoldens(t *testing.T) {
	busy := busySnapshot()
	tests := []struct {
		name     string
		width    int
		height   int
		snapshot *telemetry.Snapshot
		want     []string
		notWant  []string
	}{
		{
			name:   "waiting",
			width:  80,
			height: 24,
			want:   []string{"Waiting for telemetry snapshot", "logs /var/log/detent.log"},
		},
		{
			name:     "busy_120x40",
			width:    120,
			height:   40,
			snapshot: &busy,
			want:     []string{"RUNNING (7)", "PID", "SESSION", "COMPLETED (10)", "logs /var/log/detent.log"},
		},
		{
			name:     "narrow_80x24",
			width:    80,
			height:   24,
			snapshot: &busy,
			want:     []string{"RUNNING (7, showing 4)", "+3 more", "EVENT"},
			notWant:  []string{"PID", "SESSION"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := NewModel(
				context.Background(),
				hub.New[telemetry.Snapshot](),
				WithNow(func() time.Time { return time.Date(2026, 5, 31, 0, 15, 30, 0, time.UTC) }),
				WithBuild(buildinfo.Info{Version: "v0.32.0", Commit: "abcdef1234567890", Date: "2026-06-05T21:00:00Z"}),
				WithLogPath(" /var/log/detent.log "),
			)
			if err != nil {
				t.Fatalf("NewModel() error = %v", err)
			}
			t.Cleanup(model.Close)

			next, _ := model.Update(tea.WindowSizeMsg{Width: tt.width, Height: tt.height})
			model = next.(Model)
			if tt.snapshot != nil {
				next, _ = model.Update(snapshotMsg{snapshot: *tt.snapshot})
				model = next.(Model)
			}

			view := model.View()
			if !view.AltScreen {
				t.Fatal("View().AltScreen = false, want true")
			}
			if !view.DisableBracketedPasteMode {
				t.Fatal("View().DisableBracketedPasteMode = false, want true")
			}

			got := ansi.Strip(view.Content)
			assertScreenSize(t, got, tt.width, tt.height)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("View() missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("View() unexpectedly contains %q:\n%s", notWant, got)
				}
			}
			assertGolden(t, filepath.Join("testdata", tt.name+".golden"), got)
		})
	}
}

func TestModelRendersSnapshotFromHub(t *testing.T) {
	t.Parallel()

	snapshots := hub.New[telemetry.Snapshot]()
	model, err := NewModel(context.Background(), snapshots)
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	cmd := model.Init()
	if err := snapshots.Publish(testSnapshot()); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	msg := cmd()
	next, nextCmd := model.Update(msg)
	if nextCmd == nil {
		t.Fatal("Update() did not return a follow-up subscription command")
	}
	view := ansi.Strip(next.(Model).View().Content)
	for _, want := range []string{"RUNNING (1)", "DD-44", "turn completed", "QUEUE (1)", "BLOCKED (1)", "COMPLETED (1)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() missing %q:\n%s", want, view)
		}
	}
}

func TestAssertGoldenAcceptsWindowsLineEndings(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "windows.golden")
	if err := os.WriteFile(path, []byte("first\r\nsecond"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	assertGolden(t, path, "first\nsecond")
}

func TestModelRendersDrainingShutdown(t *testing.T) {
	t.Parallel()

	requestedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	snapshot := testSnapshot()
	snapshot.Shutdown = telemetry.Shutdown{
		Status:            "draining",
		Draining:          true,
		SessionsRemaining: 2,
		RequestedAt:       &requestedAt,
	}
	model := Model{
		snapshot:              snapshot,
		hasSnapshot:           true,
		width:                 160,
		height:                40,
		now:                   func() time.Time { return requestedAt.Add(15 * time.Second) },
		shutdownTimeoutSource: func() time.Duration { return 75 * time.Second },
		styles:                newStyles(),
	}

	view := ansi.Strip(model.View().Content)
	want := "Shutdown: draining (2 sessions remaining, 1m 0s until force quit; press Ctrl+C again to force quit immediately)"
	if !strings.Contains(view, want) {
		t.Fatalf("View() missing %q:\n%s", want, view)
	}
}

func TestRunningTableColumnsRespondToWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		width int
		want  string
	}{
		{name: "narrow", width: 80, want: "ID,STAGE,AGE / TURN,TOKENS/CTX,EVENT"},
		{name: "medium", width: 100, want: "ID,STAGE,AGE / TURN,TOKENS/CTX,SESSION,EVENT"},
		{name: "wide", width: 120, want: "ID,STAGE,PID,AGE / TURN,TOKENS/CTX,SESSION,EVENT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			columns := runningTableColumns(tt.width-4, tt.width)
			titles := make([]string, 0, len(columns))
			totalWidth := 0
			for _, column := range columns {
				titles = append(titles, column.Title)
				totalWidth += column.Width
			}
			if got := strings.Join(titles, ","); got != tt.want {
				t.Fatalf("columns = %q, want %q", got, tt.want)
			}
			if totalWidth != tt.width-4 {
				t.Fatalf("column width = %d, want %d", totalWidth, tt.width-4)
			}
		})
	}
}

func TestRunningTableRowsUseCompactIssueKeyAndContextPressure(t *testing.T) {
	t.Parallel()

	contextWindow := int64(100)
	tests := []struct {
		name    string
		issue   telemetry.Issue
		tokens  telemetry.Tokens
		want    []string
		notWant []string
	}{
		{
			name:    "github identifier",
			issue:   telemetry.Issue{ID: "opaque", Identifier: "digitaldrywood/detent#402"},
			want:    []string{"#402"},
			notWant: []string{"digit..."},
		},
		{
			name:   "linear identifier and context pressure",
			issue:  telemetry.Issue{ID: "opaque", Identifier: "DD-977"},
			tokens: telemetry.Tokens{Total: 90, ModelContextWindow: &contextWindow},
			want:   []string{"DD-977", "90/90%"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := Model{styles: newStyles()}
			rows, _ := model.runningTableRows([]telemetry.Running{{
				Issue:          tt.issue,
				LastEvent:      "turn_completed",
				LastMessage:    "latest event",
				RuntimeSeconds: 12,
				Tokens:         tt.tokens,
			}}, 80)
			got := ansi.Strip(strings.Join(rows[0], " "))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("running row missing %q: %s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("running row unexpectedly contains %q: %s", notWant, got)
				}
			}
		})
	}
}

func TestModelHandlesWindowSize(t *testing.T) {
	t.Parallel()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	next, _ := model.Update(tea.WindowSizeMsg{Width: 72, Height: 31})
	got := next.(Model)
	if got.width != 72 || got.height != 31 {
		t.Fatalf("window size = %dx%d, want 72x31", got.width, got.height)
	}
}

func TestModelClosesSubscriptionOnQuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{name: "q", msg: tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"})},
		{name: "escape", msg: tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
			if err != nil {
				t.Fatalf("NewModel() error = %v", err)
			}
			if _, cmd := model.Update(tt.msg); cmd == nil {
				t.Fatalf("Update(%s) did not return quit command", tt.name)
			}
			assertSubscriptionClosed(t, model)
		})
	}
}

func TestModelRequestsShutdownForInterruptMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.Msg
	}{
		{name: "signal", msg: tea.InterruptMsg{}},
		{name: "key press", msg: tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			interrupts := 0
			model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot](), WithInterruptFunc(func() {
				interrupts++
			}))
			if err != nil {
				t.Fatalf("NewModel() error = %v", err)
			}
			t.Cleanup(model.Close)

			next, cmd := model.Update(tt.msg)
			if cmd == nil {
				t.Fatal("Update() did not return shutdown command")
			}
			got := next.(Model)
			if got.shutdownNote != shutdownDrainNotice {
				t.Fatalf("shutdown note = %q, want %q", got.shutdownNote, shutdownDrainNotice)
			}
			if interrupts != 0 {
				t.Fatalf("interrupts = %d before command runs, want 0", interrupts)
			}
			next, nextCmd := got.Update(cmd())
			if nextCmd != nil {
				t.Fatal("shutdownInterruptMsg returned a command")
			}
			if interrupts != 1 {
				t.Fatalf("interrupts = %d, want 1", interrupts)
			}
			if next.(Model).shutdownNote != shutdownDrainNotice {
				t.Fatal("shutdown notice did not persist")
			}
		})
	}
}

func TestModelSecondInterruptRequestsForceQuit(t *testing.T) {
	t.Parallel()

	interrupts := 0
	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot](), WithInterruptFunc(func() {
		interrupts++
	}))
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	next, cmd := model.Update(tea.InterruptMsg{})
	model = next.(Model)
	next, _ = model.Update(cmd())
	model = next.(Model)
	next, cmd = model.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	model = next.(Model)
	if model.shutdownNote != shutdownForceNotice {
		t.Fatalf("shutdown note = %q, want %q", model.shutdownNote, shutdownForceNotice)
	}
	if interrupts != 1 {
		t.Fatalf("interrupts = %d before force command runs, want 1", interrupts)
	}
	model.Update(cmd())
	if interrupts != 2 {
		t.Fatalf("interrupts = %d, want 2", interrupts)
	}
}

func TestModelShowsShutdownNoticeBeforeSnapshot(t *testing.T) {
	t.Parallel()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot](), WithInterruptFunc(func() {}))
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)

	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	model = next.(Model)
	next, cmd := model.Update(tea.InterruptMsg{})
	if cmd == nil {
		t.Fatal("Update() did not return shutdown command")
	}
	view := ansi.Strip(next.(Model).View().Content)
	if !strings.Contains(view, "Shutdown: "+shutdownDrainNotice) {
		t.Fatalf("View() missing shutdown notice:\n%s", view)
	}
}

func TestModelQuitsOnInterruptWithoutHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.Msg
	}{
		{name: "signal", msg: tea.InterruptMsg{}},
		{name: "key press", msg: tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot]())
			if err != nil {
				t.Fatalf("NewModel() error = %v", err)
			}
			if _, cmd := model.Update(tt.msg); cmd == nil {
				t.Fatal("Update() did not return quit command")
			}
			assertSubscriptionClosed(t, model)
		})
	}
}

func TestNewModelRejectsNilHub(t *testing.T) {
	t.Parallel()

	if _, err := NewModel(context.Background(), nil); err == nil {
		t.Fatal("NewModel(nil) error = nil, want error")
	}
}

func assertScreenSize(t *testing.T, screen string, width int, height int) {
	t.Helper()

	lines := strings.Split(screen, "\n")
	if len(lines) != height {
		t.Fatalf("screen height = %d, want %d", len(lines), height)
	}
	for lineNumber, line := range lines {
		if got := ansi.StringWidth(line); got != width {
			t.Fatalf("line %d width = %d, want %d: %q", lineNumber+1, got, width, line)
		}
	}
}

func assertGolden(t *testing.T, path string, got string) {
	t.Helper()

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v; run go test ./internal/tui -update", path, err)
	}
	wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
	if got != wantText {
		t.Fatalf("rendered view differs from %s; run go test ./internal/tui -update\nwant:\n%s\ngot:\n%s", path, wantText, got)
	}
}

func assertSubscriptionClosed(t *testing.T, model Model) {
	t.Helper()

	select {
	case _, ok := <-model.subscription.C():
		if ok {
			t.Fatal("subscription channel is open")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription channel to close")
	}
}

func busySnapshot() telemetry.Snapshot {
	snapshot := testSnapshot()
	snapshot.Running = make([]telemetry.Running, 7)
	for i := range snapshot.Running {
		row := testSnapshot().Running[0]
		row.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", 482-i)
		row.ProcessIdentity = strconv.Itoa(4200 + i)
		row.SessionID = fmt.Sprintf("019f0000-0000-0000-0000-%012d", i)
		row.LastMessage = fmt.Sprintf("turn %d completed", i+1)
		row.TurnCount = i + 1
		row.Tokens.Total = int64((i + 1) * 45120)
		snapshot.Running[i] = row
	}
	snapshot.Queue = make([]telemetry.Queued, 5)
	for i := range snapshot.Queue {
		row := testSnapshot().Queue[0]
		row.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", 470-i)
		row.Attempt = i + 1
		row.DueInMillis = int64((i + 1) * 1500)
		snapshot.Queue[i] = row
	}
	snapshot.Blocked = make([]telemetry.Blocked, 5)
	for i := range snapshot.Blocked {
		row := testSnapshot().Blocked[0]
		row.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", 460-i)
		row.Error = fmt.Sprintf("blocked by dependency #%d", 900+i)
		snapshot.Blocked[i] = row
	}
	snapshot.Completed = make([]telemetry.Completed, 10)
	for i := range snapshot.Completed {
		row := testSnapshot().Completed[0]
		row.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", 450-i)
		row.CompletedAt = row.CompletedAt.Add(-time.Duration(i) * time.Minute)
		row.Tokens.Total = int64((i + 1) * 88410)
		snapshot.Completed[i] = row
	}
	snapshot.Counts = telemetry.Counts{Running: 7, Queue: 5, Blocked: 5, Completed: 10}
	return snapshot
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
			NextRefreshAt:       new(generatedAt.Add(30 * time.Second)),
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
					Title:      "feat(tui): full-screen dashboard",
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
