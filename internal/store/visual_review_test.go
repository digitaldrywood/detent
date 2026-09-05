package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/visualreview"
)

func TestVisualReviewPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db")
	first := openVisualReviewTestStore(t, path)
	capture := visualReviewTestCapture()
	if err := first.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}
	wantDraft, err := first.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, capture.HeadSHA, 0, []byte(`{"status":"commented"}`), "reviewer@example.com", time.Date(2026, 9, 4, 12, 30, 0, 0, time.FixedZone("test", -5*60*60)))
	if err != nil {
		t.Fatalf("SaveVisualReviewDraft() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	restarted := openVisualReviewTestStore(t, path)
	t.Cleanup(func() { _ = restarted.Close() })
	gotCapture, err := restarted.VisualReviewCapture(t.Context(), capture.ProjectID, capture.CaptureID)
	if err != nil {
		t.Fatalf("VisualReviewCapture() after restart error = %v", err)
	}
	if gotCapture.IssueID != capture.IssueID || gotCapture.HeadSHA != capture.HeadSHA || !bytes.Equal(gotCapture.ManifestJSON, capture.ManifestJSON) {
		t.Fatalf("VisualReviewCapture() after restart = %+v, want issue %q, head %q, manifest %s", gotCapture, capture.IssueID, capture.HeadSHA, capture.ManifestJSON)
	}
	if len(gotCapture.Assets) != 1 || gotCapture.Assets[0].ID != capture.Assets[0].ID || gotCapture.Assets[0].StorageKey != capture.Assets[0].StorageKey {
		t.Fatalf("VisualReviewCapture().Assets after restart = %+v, want %+v", gotCapture.Assets, capture.Assets)
	}
	gotDraft, err := restarted.VisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID)
	if err != nil {
		t.Fatalf("VisualReviewDraft() after restart error = %v", err)
	}
	assertVisualReviewDraft(t, gotDraft, wantDraft)
}

func TestVisualReviewConcurrentExpectedRevisionAllowsOneSave(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, feedback := range [][]byte{[]byte(`{"comment":"first"}`), []byte(`{"comment":"second"}`)} {
		feedback := feedback
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := backend.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, capture.HeadSHA, 0, feedback, "reviewer", time.Now())
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, visualreview.ErrConflict):
			conflicts++
		default:
			t.Fatalf("SaveVisualReviewDraft() error = %v, want nil or ErrConflict", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent saves produced %d successes and %d conflicts, want 1 each", successes, conflicts)
	}
}

func TestVisualReviewSaveReturnsOwnRevision(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}

	firstJSON := []byte(`{"comment":"first"}`)
	wantFirstJSON := append([]byte(nil), firstJSON...)
	first, err := backend.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, capture.HeadSHA, 0, firstJSON, "first-writer", time.Now())
	if err != nil {
		t.Fatalf("first SaveVisualReviewDraft() error = %v", err)
	}
	firstJSON[0] = '['
	if first.FeedbackJSON[0] != '{' {
		t.Fatalf("first save returned caller-owned FeedbackJSON %q", first.FeedbackJSON)
	}
	second, err := backend.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, capture.HeadSHA, first.Revision, []byte(`{"comment":"second"}`), "later-writer", time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("second SaveVisualReviewDraft() error = %v", err)
	}
	if first.Revision != 1 || first.AuditActor != "first-writer" || !bytes.Equal(first.FeedbackJSON, wantFirstJSON) {
		t.Fatalf("first save returned %+v after later writer, want its own revision 1 and payload", first)
	}
	if second.Revision != 2 {
		t.Fatalf("second save revision = %d, want 2", second.Revision)
	}
}

func TestVisualReviewRejectsWrongProjectAndHead(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}

	_, err := backend.SaveVisualReviewDraft(t.Context(), "other-project", capture.CaptureID, capture.HeadSHA, 0, []byte(`{}`), "reviewer", time.Now())
	if !errors.Is(err, visualreview.ErrNotFound) {
		t.Fatalf("SaveVisualReviewDraft(wrong project) error = %v, want ErrNotFound", err)
	}
	_, err = backend.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, "other-head", 0, []byte(`{}`), "reviewer", time.Now())
	if !errors.Is(err, visualreview.ErrConflict) {
		t.Fatalf("SaveVisualReviewDraft(wrong head) error = %v, want ErrConflict", err)
	}
	if _, err := backend.VisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID); err != nil {
		t.Fatalf("VisualReviewDraft() after rejected saves error = %v", err)
	}
}

func TestVisualReviewCaptureIDIsImmutableAcrossIssues(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}
	capture.IssueID = "issue-99"
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); !errors.Is(err, visualreview.ErrConflict) {
		t.Fatalf("CreateVisualReviewCapture(same ID, different issue) error = %v, want ErrConflict", err)
	}
}

func TestVisualReviewCaptureIDIsImmutableAcrossAssetBindings(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}

	for _, mutate := range []struct {
		name string
		do   func(*visualreview.Capture)
	}{
		{name: "hash", do: func(c *visualreview.Capture) { c.Assets[0].SHA256 = "different" }},
		{name: "storage key", do: func(c *visualreview.Capture) { c.Assets[0].StorageKey = "different/key.png" }},
		{name: "asset list", do: func(c *visualreview.Capture) { c.Assets = nil }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			changed := capture
			changed.Assets = append([]visualreview.Asset(nil), capture.Assets...)
			mutate.do(&changed)
			if err := backend.CreateVisualReviewCapture(t.Context(), changed); !errors.Is(err, visualreview.ErrConflict) {
				t.Fatalf("CreateVisualReviewCapture() error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestVisualReviewLatestUsesInsertionOrderForEqualTimestamps(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	first := visualReviewTestCapture()
	first.CaptureID = "z-first"
	first.Assets[0].StorageKey = "visual-review/project-1/z-first/asset-1.png"
	second := visualReviewTestCapture()
	second.CaptureID = "a-second"
	second.Assets[0].StorageKey = "visual-review/project-1/a-second/asset-1.png"
	if err := backend.CreateVisualReviewCapture(t.Context(), first); err != nil {
		t.Fatalf("CreateVisualReviewCapture(first) error = %v", err)
	}
	if err := backend.CreateVisualReviewCapture(t.Context(), second); err != nil {
		t.Fatalf("CreateVisualReviewCapture(second) error = %v", err)
	}

	latest, err := backend.LatestVisualReviewCapture(t.Context(), first.ProjectID, first.IssueID)
	if err != nil {
		t.Fatalf("LatestVisualReviewCapture() error = %v", err)
	}
	if latest.CaptureID != second.CaptureID {
		t.Fatalf("LatestVisualReviewCapture().CaptureID = %q, want later insertion %q", latest.CaptureID, second.CaptureID)
	}
	listed, err := backend.ListVisualReviewCaptures(t.Context(), first.ProjectID, first.IssueID)
	if err != nil {
		t.Fatalf("ListVisualReviewCaptures() error = %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("ListVisualReviewCaptures() count = %d, want 2", len(listed))
	}
	if listed[0].CaptureID != second.CaptureID || listed[1].CaptureID != first.CaptureID {
		t.Fatalf("ListVisualReviewCaptures() IDs = [%s, %s], want [%s, %s]", listed[0].CaptureID, listed[1].CaptureID, second.CaptureID, first.CaptureID)
	}
}

func TestVisualReviewDraftInsertPropagatesNonUniqueConstraintError(t *testing.T) {
	t.Parallel()

	backend := openVisualReviewTestStore(t, filepath.Join(t.TempDir(), "detent.db"))
	t.Cleanup(func() { _ = backend.Close() })
	capture := visualReviewTestCapture()
	if err := backend.CreateVisualReviewCapture(t.Context(), capture); err != nil {
		t.Fatalf("CreateVisualReviewCapture() error = %v", err)
	}
	sqliteBackend := backend.(*sqliteStore)
	if _, err := sqliteBackend.db.ExecContext(t.Context(), `CREATE TRIGGER reject_visual_review_draft BEFORE INSERT ON visual_review_drafts BEGIN SELECT RAISE(FAIL, 'draft rejected'); END`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}

	_, err := backend.SaveVisualReviewDraft(t.Context(), capture.ProjectID, capture.CaptureID, capture.HeadSHA, 0, []byte(`{}`), "reviewer", time.Now())
	if err == nil {
		t.Fatal("SaveVisualReviewDraft() error = nil, want trigger error")
	}
	if errors.Is(err, visualreview.ErrConflict) {
		t.Fatalf("SaveVisualReviewDraft() error = %v, must not classify trigger failure as ErrConflict", err)
	}
}

func openVisualReviewTestStore(t *testing.T, path string) Store {
	t.Helper()
	backend, err := Open(t.Context(), Config{Backend: BackendSQLite, Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return backend
}

func visualReviewTestCapture() visualreview.Capture {
	return visualreview.Capture{
		ProjectID:    "project-1",
		IssueID:      "issue-42",
		Repository:   "digitaldrywood/detent",
		PR:           123,
		CaptureID:    "capture-1",
		HeadSHA:      "head-sha",
		BaseSHA:      "base-sha",
		CapturedAt:   time.Date(2026, 9, 4, 16, 0, 0, 123, time.UTC),
		Title:        "Dashboard",
		Summary:      "Visual review summary",
		ManifestJSON: []byte(`{"schema":1}`),
		Assets: []visualreview.Asset{{
			ID: "asset-1", StorageKey: "visual-review/project-1/capture-1/asset-1.png", Kind: "screenshot",
			MediaType: "image/png", SizeBytes: 1234, SHA256: "abc123", Width: 800, Height: 600,
		}},
		CreatedAt: time.Date(2026, 9, 4, 16, 1, 0, 456, time.UTC),
	}
}

func assertVisualReviewDraft(t *testing.T, got, want visualreview.Draft) {
	t.Helper()
	if got.ProjectID != want.ProjectID || got.CaptureID != want.CaptureID || got.HeadSHA != want.HeadSHA || got.Revision != want.Revision || got.AuditActor != want.AuditActor || !got.UpdatedAt.Equal(want.UpdatedAt) || !bytes.Equal(got.FeedbackJSON, want.FeedbackJSON) {
		t.Fatalf("VisualReviewDraft() = %+v, want %+v", got, want)
	}
}
