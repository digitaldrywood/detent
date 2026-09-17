package web

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestSnapshotOpenQuestions(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "questions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{store: db, logger: slog.Default()}
	questions := db.(store.HumanQuestionStore)
	now := time.Now().UTC()
	for _, tc := range []struct {
		project string
		ago     time.Duration
	}{{"a", time.Hour}, {"z", 10 * time.Hour}} {
		q := store.HumanQuestion{ProjectID: tc.project, IssueID: "1", Identifier: "owner/repo#1", Key: "route", Body: "Route upstream?", QuestionCommentID: "123", AskedAt: new(now.Add(-tc.ago))}
		if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
			t.Fatal(err)
		}
		if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := server.snapshotOpenQuestions(t.Context(), telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ProjectID: "z", Identifier: "owner/repo#1", URL: "https://git.example.com/owner/repo/issues/1"}}})
	response := snapshot.WithFreshness(now)
	if len(response.OpenQuestions) != 2 || response.OpenQuestions[0].ProjectID != "z" || *response.OpenQuestions[0].AgeSeconds != 36000 {
		t.Fatalf("questions = %+v", response.OpenQuestions)
	}
	if response.OpenQuestions[0].URL != "https://git.example.com/owner/repo/issues/1#issuecomment-123" {
		t.Fatalf("URL = %s", response.OpenQuestions[0].URL)
	}
}

func TestSnapshotOmitsClosureResolvedQuestions(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "questions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	questions := db.(store.HumanQuestionStore)
	q := store.HumanQuestion{ProjectID: "p", IssueID: "i", Identifier: "owner/repo#1", Key: "target", Body: "Which target?", QuestionCommentID: "123"}
	if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, logger: slog.Default()}
	snapshot := server.snapshotOpenQuestions(t.Context(), telemetry.Snapshot{})
	if len(snapshot.OpenQuestions) != 1 {
		t.Fatal("missing open question")
	}
	if err := questions.ResolveHumanQuestionsByClosure(t.Context(), q.ProjectID, q.IssueID); err != nil {
		t.Fatal(err)
	}
	snapshot = server.snapshotOpenQuestions(t.Context(), snapshot)
	if len(snapshot.OpenQuestions) != 0 {
		t.Fatal("closure retained stale card question facts")
	}
}
