package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestProjectDispatchStatusRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	allSkippedSince := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	lastSelectedAt := allSkippedSince.Add(-3 * time.Hour)
	observedAt := allSkippedSince.Add(2 * time.Hour)
	want := ProjectDispatchStatus{
		ProjectID:              "detent",
		CandidateCount:         8,
		EligibleCandidateCount: 3,
		CandidateFingerprint:   "candidate-set",
		SkippedCount:           8,
		WaitReason:             "github_rest_capacity",
		WaitReasonCode:         "github_rest_capacity_paused",
		AllSkippedSince:        &allSkippedSince,
		LastSelectedAt:         &lastSelectedAt,
		ObservedAt:             observedAt,
	}

	if err := backend.RecordProjectDispatchStatus(ctx, want); err != nil {
		t.Fatalf("RecordProjectDispatchStatus() error = %v", err)
	}
	got, err := backend.ProjectDispatchStatus(ctx, "detent")
	if err != nil {
		t.Fatalf("ProjectDispatchStatus() error = %v", err)
	}
	if got.ProjectID != want.ProjectID || got.CandidateCount != want.CandidateCount || got.EligibleCandidateCount != want.EligibleCandidateCount || got.CandidateFingerprint != want.CandidateFingerprint || got.SelectedCount != 0 || got.SkippedCount != want.SkippedCount || got.WaitReason != want.WaitReason || got.WaitReasonCode != want.WaitReasonCode {
		t.Fatalf("ProjectDispatchStatus() = %#v, want %#v", got, want)
	}
	if got.AllSkippedSince == nil || !got.AllSkippedSince.Equal(allSkippedSince) {
		t.Fatalf("AllSkippedSince = %#v, want %s", got.AllSkippedSince, allSkippedSince)
	}
	if got.LastSelectedAt == nil || !got.LastSelectedAt.Equal(lastSelectedAt) {
		t.Fatalf("LastSelectedAt = %#v, want %s", got.LastSelectedAt, lastSelectedAt)
	}
	if !got.ObservedAt.Equal(observedAt) {
		t.Fatalf("ObservedAt = %s, want %s", got.ObservedAt, observedAt)
	}

	selectedAt := observedAt.Add(time.Minute)
	if err := backend.RecordProjectDispatchStatus(ctx, ProjectDispatchStatus{
		ProjectID:              "detent",
		CandidateCount:         1,
		EligibleCandidateCount: 1,
		SelectedCount:          1,
		LastSelectedAt:         &selectedAt,
		ObservedAt:             selectedAt,
	}); err != nil {
		t.Fatalf("RecordProjectDispatchStatus() update error = %v", err)
	}
	got, err = backend.ProjectDispatchStatus(ctx, "detent")
	if err != nil {
		t.Fatalf("ProjectDispatchStatus() after update error = %v", err)
	}
	if got.CandidateCount != 1 || got.EligibleCandidateCount != 1 || got.SelectedCount != 1 || got.SkippedCount != 0 || got.WaitReason != "" || got.WaitReasonCode != "" || got.AllSkippedSince != nil {
		t.Fatalf("ProjectDispatchStatus() after selection = %#v", got)
	}
	if got.LastSelectedAt == nil || !got.LastSelectedAt.Equal(selectedAt) {
		t.Fatalf("LastSelectedAt after selection = %#v, want %s", got.LastSelectedAt, selectedAt)
	}
}

func TestProjectDispatchStatusRequiresProject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := openTestStore(t, ctx)
	if err := backend.RecordProjectDispatchStatus(ctx, ProjectDispatchStatus{ObservedAt: time.Now()}); err == nil {
		t.Fatal("RecordProjectDispatchStatus() error = nil, want validation error")
	}
	if _, err := backend.ProjectDispatchStatus(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ProjectDispatchStatus() error = %v, want ErrNotFound", err)
	}
}

func TestProjectDispatchStatusMigrationBackfillsLastSelection(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "detent.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})
	if err := configureSQLite(ctx, db, 0); err != nil {
		t.Fatalf("configureSQLite() error = %v", err)
	}

	migrationMu.Lock()
	defer migrationMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect() error = %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 29); err != nil {
		t.Fatalf("goose.UpToContext(29) error = %v", err)
	}
	selectedAt := "2026-08-10T00:05:48Z"
	observedAt := "2026-08-12T16:46:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO scheduler_decisions (project_id, identifier, result, selected, decision_at)
		VALUES ('detent', 'digitaldrywood/detent#1', 'selected', 1, ?),
		       ('detent', 'digitaldrywood/detent#2', 'skipped', 0, ?)
	`, selectedAt, observedAt); err != nil {
		t.Fatalf("seed scheduler_decisions error = %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 32); err != nil {
		t.Fatalf("goose.UpToContext(32) error = %v", err)
	}

	var gotSelectedAt string
	var gotObservedAt string
	if err := db.QueryRowContext(ctx, `SELECT last_selected_at, observed_at FROM project_dispatch_status WHERE project_id = 'detent'`).Scan(&gotSelectedAt, &gotObservedAt); err != nil {
		t.Fatalf("read backfilled dispatch status error = %v", err)
	}
	if gotSelectedAt != selectedAt || gotObservedAt != observedAt {
		t.Fatalf("backfilled timestamps = %q/%q, want %q/%q", gotSelectedAt, gotObservedAt, selectedAt, observedAt)
	}
}
