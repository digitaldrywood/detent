package cli

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestDoctorParkReviewThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		parks     int
		threshold int
		want      doctorStatus
	}{
		{name: "below threshold", parks: 2, threshold: 3, want: doctorOK},
		{name: "at threshold", parks: 3, threshold: 3, want: doctorWarn},
		{name: "higher configured threshold", parks: 3, threshold: 4, want: doctorOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := createDoctorParkStore(t)
			insertDoctorParks(t, path, 0, tt.parks)
			check := checkDoctorParkReview(t.Context(), "detent", path, tt.threshold, doctorDeps{})
			if check.Status != tt.want {
				t.Fatalf("Status = %s, want %s: %#v", check.Status, tt.want, check)
			}
			if tt.want == doctorWarn && (len(check.ParkReviews) != 1 || !containsAll(check.Detail, "review recommended", "3 parks")) {
				t.Fatalf("warning = %#v", check)
			}
		})
	}
}

func TestDoctorParkReviewListsEveryIssue(t *testing.T) {
	t.Parallel()

	path := createDoctorParkStore(t)
	insertDoctorParks(t, path, 0, 3)
	insertDoctorIssueParks(t, path, "issue-1774", "digitaldrywood/detent#1774", 3, 4)
	check := checkDoctorParkReview(t.Context(), "detent", path, 3, doctorDeps{})
	if check.Status != doctorWarn || len(check.ParkReviews) != 2 {
		t.Fatalf("check = %#v, want two review recommendations", check)
	}
	for _, want := range []string{"digitaldrywood/detent#1773 (3 parks)", "digitaldrywood/detent#1774 (4 parks)"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
		}
	}
}

func TestDoctorParkReviewAcknowledgementClearsAndRearms(t *testing.T) {
	t.Parallel()

	path := createDoctorParkStore(t)
	insertDoctorParks(t, path, 0, 3)
	if check := checkDoctorParkReview(t.Context(), "detent", path, 3, doctorDeps{}); check.Status != doctorWarn {
		t.Fatalf("initial status = %s, want WARN", check.Status)
	}
	backend, err := store.Open(t.Context(), store.Config{Path: path})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	parks := backend.(store.ParkSummaryStore)
	identity := store.IssueIdentity{ProjectID: "detent", IssueID: "issue-1773", Identifier: "digitaldrywood/detent#1773"}
	if err := parks.AcknowledgeIssueParks(t.Context(), identity, 3, time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("AcknowledgeIssueParks() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if check := checkDoctorParkReview(t.Context(), "detent", path, 3, doctorDeps{}); check.Status != doctorOK {
		t.Fatalf("acknowledged status = %s, want OK: %#v", check.Status, check)
	}
	insertDoctorParks(t, path, 3, 1)
	if check := checkDoctorParkReview(t.Context(), "detent", path, 3, doctorDeps{}); check.Status != doctorWarn {
		t.Fatalf("rearmed status = %s, want WARN: %#v", check.Status, check)
	}
}

func createDoctorParkStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "detent.db")
	backend, err := store.Open(t.Context(), store.Config{Path: path})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return path
}

func insertDoctorParks(t *testing.T, path string, offset, count int) {
	t.Helper()
	insertDoctorIssueParks(t, path, "issue-1773", "digitaldrywood/detent#1773", offset, count)
}

func insertDoctorIssueParks(t *testing.T, path, issueID, identifier string, offset, count int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	for index := range count {
		at := time.Date(2026, 8, 9, 16+offset+index, 0, 0, 0, time.UTC)
		if _, err := db.Exec(`INSERT INTO work_attempts (
project_id, issue_id, identifier, worker_type, attempt_number, status, started_at, completed_at, terminal_state, error_class
) VALUES ('detent', ?, ?, 'codex', ?, 'terminal', ?, ?, 'no_progress', 'no_progress_limit')`, issueID, identifier, offset+index+1, at.Add(-time.Minute).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("insert park %d: %v", index, err)
		}
	}
}

func containsAll(value string, expected ...string) bool {
	for _, item := range expected {
		if !strings.Contains(value, item) {
			return false
		}
	}
	return true
}
