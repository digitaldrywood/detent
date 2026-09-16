package web

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStateOpenQuestions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-6 * time.Hour)
	for _, tc := range []struct {
		name string
		at   *time.Time
		want *int64
	}{
		{"known", &at, new(int64(21600))}, {"legacy", nil, nil}, {"clock skew", new(now.Add(time.Hour)), new(int64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := telemetry.Snapshot{GeneratedAt: now, Project: telemetry.Project{ID: "p"}, OpenQuestions: []operations.Decision{{ProjectID: "p", Issue: "owner/repo#1", Question: "Route upstream?", AskedAt: tc.at}, {ProjectID: "other", Issue: "owner/repo#2"}}}
			response := stateResponse(snapshot, now, now, "test", buildinfo.Info{})
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				OpenQuestions []operations.Decision `json:"open_questions"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.OpenQuestions) != 2 {
				t.Fatalf("questions = %+v", decoded)
			}
			q := decoded.OpenQuestions[0]
			if q.Question != "Route upstream?" || (q.AgeSeconds == nil) != (tc.want == nil) || tc.want != nil && *q.AgeSeconds != *tc.want {
				t.Fatalf("question = %+v", q)
			}
			scoped, ok := projectScopedSnapshot(snapshot, "p")
			if !ok || len(scoped.OpenQuestions) != 1 || scoped.OpenQuestions[0].ProjectID != "p" {
				t.Fatalf("scope = %+v", scoped.OpenQuestions)
			}
			if snapshot.OpenQuestions[0].AgeSeconds != nil {
				t.Fatal("mutated source snapshot")
			}
		})
	}
}

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
	response := stateResponse(snapshot, now, now, "test", buildinfo.Info{})
	if len(response.OpenQuestions) != 2 || response.OpenQuestions[0].ProjectID != "z" || *response.OpenQuestions[0].AgeSeconds != 36000 {
		t.Fatalf("questions = %+v", response.OpenQuestions)
	}
	if response.OpenQuestions[0].URL != "https://git.example.com/owner/repo/issues/1#issuecomment-123" {
		t.Fatalf("URL = %s", response.OpenQuestions[0].URL)
	}
}
