package local

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func newCaseTestConnector(t *testing.T, issues ...connector.Issue) *Connector {
	t.Helper()
	conn, err := New(Config{
		Path:           filepath.Join(t.TempDir(), "work-items.db"),
		ProjectID:      "video",
		Issues:         issues,
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		Now: func() time.Time {
			return time.Date(2026, 7, 8, 22, 46, 22, 502102357, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func seedIssue(id, state string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = id
	issue.Title = "Rework Storyboard"
	issue.State = state
	return issue
}

// Regression test for #1067: the scheduler lowercases configured states while
// rows hold the template's capitalized spellings; the state filter must match
// either way.
func TestFetchIssuesByStatesMatchesCaseInsensitively(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newCaseTestConnector(t, seedIssue("wi-1", "Todo"))

	tests := []struct {
		name   string
		states []string
		want   int
	}{
		{name: "lowercased query against capitalized row", states: []string{"todo", "production", "rework"}, want: 1},
		{name: "capitalized query", states: []string{"Todo"}, want: 1},
		{name: "uppercase query", states: []string{"TODO"}, want: 1},
		{name: "non-matching state", states: []string{"review"}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issues, err := conn.FetchIssuesByStates(ctx, tt.states)
			if err != nil {
				t.Fatalf("FetchIssuesByStates(%v) error = %v", tt.states, err)
			}
			if len(issues) != tt.want {
				t.Fatalf("FetchIssuesByStates(%v) len = %d, want %d", tt.states, len(issues), tt.want)
			}
		})
	}
}

func TestUpdateIssueStateWritesConfiguredSpelling(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newCaseTestConnector(t, seedIssue("wi-1", "Todo"))

	// The orchestrator writes lowercased state names; the stored value must
	// keep the configured spelling so the column stays single-cased.
	if err := conn.UpdateIssueState(ctx, "wi-1", "blocked"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	var stored string
	if err := conn.db.QueryRowContext(ctx, "select state from detent_work_items where id = 'wi-1'").Scan(&stored); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if stored != "Blocked" {
		t.Fatalf("stored state = %q, want Blocked", stored)
	}

	// A state outside the configured vocabulary is written as passed.
	if err := conn.UpdateIssueState(ctx, "wi-1", "Custom Lane"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := conn.db.QueryRowContext(ctx, "select state from detent_work_items where id = 'wi-1'").Scan(&stored); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if stored != "Custom Lane" {
		t.Fatalf("stored state = %q, want Custom Lane", stored)
	}
}

func TestSeedNormalizesStateToConfiguredSpelling(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newCaseTestConnector(t, seedIssue("wi-lower", "todo"))

	var stored string
	if err := conn.db.QueryRowContext(ctx, "select state from detent_work_items where id = 'wi-lower'").Scan(&stored); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if stored != "Todo" {
		t.Fatalf("stored seed state = %q, want Todo", stored)
	}
}

func TestNewAppliesSQLitePragmas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn := newCaseTestConnector(t)

	var busyTimeout int64
	if err := conn.db.QueryRowContext(ctx, "pragma busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
	var journalMode string
	if err := conn.db.QueryRowContext(ctx, "pragma journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
}

func TestParseTimePointerToleratesExternalFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    time.Time
		wantNil bool
	}{
		{
			name:  "detent rfc3339 nanoseconds",
			value: "2026-07-08T22:46:22.502102357Z",
			want:  time.Date(2026, 7, 8, 22, 46, 22, 502102357, time.UTC),
		},
		{
			name:  "rfc3339 milliseconds",
			value: "2026-07-08T22:46:22.502Z",
			want:  time.Date(2026, 7, 8, 22, 46, 22, 502000000, time.UTC),
		},
		{
			name:  "sqlite current_timestamp",
			value: "2026-07-08 22:46:22",
			want:  time.Date(2026, 7, 8, 22, 46, 22, 0, time.UTC),
		},
		{
			name:  "date only",
			value: "2026-07-08",
			want:  time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC),
		},
		{name: "empty", value: "", wantNil: true},
		{name: "garbage", value: "yesterday", wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseTimePointer(tt.value)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("parseTimePointer(%q) = %v, want nil", tt.value, got)
				}
				return
			}
			if got == nil || !got.Equal(tt.want) {
				t.Fatalf("parseTimePointer(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
